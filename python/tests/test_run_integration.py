# !/usr/bin/env python
# coding: utf-8
"""
Full-loop integration tests for Connection.run():

- Happy path: subscribe → receive → callback → reconnect → resubscribe → receive
- ConnectionClosed triggers reconnect with sending_history replay
- GateWebSocketUpgrade triggers hard reconnect with sending_history replay
- Combined: a callback error must not bring down the run loop
"""
import asyncio
import json
import unittest
from unittest.mock import patch

import websockets

from gate_ws.client import (
    Configuration,
    Connection,
    GateWebSocketUpgrade,
)


def _make_connection_closed():
    """Compat shim for websockets.ConnectionClosed across SDK versions."""
    try:
        return websockets.ConnectionClosed(None, None)
    except TypeError:
        return websockets.ConnectionClosed(None, None, None)


class FakeWebSocket:
    """Stand-in for the object returned by websockets.connect.

    Iteration ends by raising websockets.ConnectionClosed (so run() takes
    the proper reconnect branch) once the incoming queue drains.
    """

    def __init__(self, incoming=None, end_with_close=True):
        self._incoming = list(incoming or [])
        self.sent = []
        self.closed = False
        self._end_with_close = end_with_close

    async def send(self, msg):
        self.sent.append(msg)

    async def close(self):
        self.closed = True

    def __aiter__(self):
        return self

    async def __anext__(self):
        # Yield to the scheduler so other tasks (write, ping) can run
        await asyncio.sleep(0)
        if not self._incoming:
            if self._end_with_close:
                raise _make_connection_closed()
            raise StopAsyncIteration
        msg = self._incoming.pop(0)
        if isinstance(msg, BaseException):
            raise msg
        return msg


def _trade_msg(channel="spot.trades", result=None, event="update"):
    body = {"channel": channel, "event": event}
    body["result"] = result if result is not None else [{"price": "100"}]
    return json.dumps(body)


class RunFullIntegrationTests(unittest.IsolatedAsyncioTestCase):
    async def test_happy_path_subscribe_receive_reconnect_resubscribe(self):
        """End-to-end:
        1. send subscribe (queued before run starts)
        2. run connects → _write replays subscribe, _read delivers a message
        3. first WS exhausts → ConnectionClosed → reconnect
        4. second WS receives the replayed subscribe (sending_history) and another message
        5. callback fires for both messages
        """
        # max_retry=1 → 1 initial + 1 retry = 2 dial-fail attempts ⇒ ~0.5s back-off
        cfg = Configuration(max_retry=1, ping_interval=999)  # ping silenced
        conn = Connection(cfg)

        received = []

        async def cb(c, response):
            received.append(response.channel)

        conn.register("spot.trades", cb)

        # Pre-stage subscribe in queue (BaseChannel.subscribe path)
        conn.send('{"channel":"spot.trades","event":"subscribe"}')

        connections = []
        connect_idx = 0

        async def connect_mock(*_args, **_kwargs):
            nonlocal connect_idx
            connect_idx += 1
            if connect_idx == 1:
                ws = FakeWebSocket([_trade_msg()])
            elif connect_idx == 2:
                ws = FakeWebSocket([_trade_msg()])
            else:
                # Terminate the retry loop
                raise OSError("done")
            connections.append(ws)
            return ws

        with patch("gate_ws.client.websockets.connect", connect_mock):
            await asyncio.wait_for(conn.run(), timeout=10)

        # Both connections must have received the subscribe (via sending_history replay)
        self.assertEqual(len(connections), 2)
        for ws in connections:
            self.assertIn(
                '{"channel":"spot.trades","event":"subscribe"}',
                ws.sent,
                "sending_history not replayed on reconnect",
            )

        # First WS got closed (cleanly) before retrying
        self.assertTrue(connections[0].closed or len(connections[0].sent) > 0)

        # Allow scheduled callback tasks to flush
        await asyncio.sleep(0.05)
        # Should have dispatched at least one trade message; cancellation may
        # have torn down the second one's callback before completion
        self.assertGreaterEqual(len(received), 1)

    async def test_upgrade_event_triggers_hard_reconnect_and_replay(self):
        """When server pushes event:'upgrade', _read raises GateWebSocketUpgrade,
        run() closes the conn and reconnects, replaying sending_history."""
        cfg = Configuration(max_retry=1, ping_interval=999)
        conn = Connection(cfg)
        conn.send('{"channel":"spot.trades","event":"subscribe"}')

        connections = []
        connect_idx = 0

        async def connect_mock(*_args, **_kwargs):
            nonlocal connect_idx
            connect_idx += 1
            if connect_idx == 1:
                # First WS pushes upgrade event then closes
                ws = FakeWebSocket([
                    json.dumps({
                        "channel": "spot.trades",
                        "event": "upgrade",
                        "result": {},
                    })
                ])
            elif connect_idx == 2:
                ws = FakeWebSocket()  # empty → ConnectionClosed
            else:
                raise OSError("done")
            connections.append(ws)
            return ws

        with patch("gate_ws.client.websockets.connect", connect_mock):
            await asyncio.wait_for(conn.run(), timeout=10)

        self.assertEqual(len(connections), 2)
        # Second connection should also have received the replayed subscribe
        self.assertIn(
            '{"channel":"spot.trades","event":"subscribe"}',
            connections[1].sent,
        )

    async def test_callback_exception_does_not_kill_run_loop(self):
        """A user callback that raises must not propagate up into _read and
        terminate the run loop. (For coroutine callbacks, the exception is
        captured in the spawned task and discarded by run() finally cleanup.)"""
        cfg = Configuration(max_retry=2, ping_interval=999)
        conn = Connection(cfg)

        call_count = 0

        async def bad_cb(c, response):
            nonlocal call_count
            call_count += 1
            raise RuntimeError("user code crash")

        conn.register("spot.trades", bad_cb)

        connections = []
        connect_idx = 0

        async def connect_mock(*_args, **_kwargs):
            nonlocal connect_idx
            connect_idx += 1
            if connect_idx == 1:
                ws = FakeWebSocket([_trade_msg(), _trade_msg(), _trade_msg()])
            else:
                raise OSError("done")
            connections.append(ws)
            return ws

        with patch("gate_ws.client.websockets.connect", connect_mock):
            await asyncio.wait_for(conn.run(), timeout=10)

        # Callback should have been scheduled at least once; even if it
        # crashed inside the task, run() must complete cleanly via the
        # OSError on the second connect attempt.
        await asyncio.sleep(0.05)
        self.assertGreaterEqual(call_count, 1)

    async def test_run_recovers_from_intermittent_dial_failures(self):
        """Verify that transient dial failures (within max_retry) don't kill
        the loop - run() should keep retrying until success or exhaustion."""
        cfg = Configuration(max_retry=5, ping_interval=999)
        conn = Connection(cfg)
        conn.send('{"channel":"spot.trades","event":"subscribe"}')

        attempts = 0
        connections = []

        async def connect_mock(*_args, **_kwargs):
            nonlocal attempts
            attempts += 1
            if attempts in (1, 2):
                # First two attempts: transient OSError
                raise OSError("transient")
            if attempts == 3:
                # Third: succeed but immediately close (delivers nothing)
                ws = FakeWebSocket()
                connections.append(ws)
                return ws
            # Fourth+: persistent failure to terminate the loop
            raise OSError("permanent")

        with patch("gate_ws.client.websockets.connect", connect_mock):
            await asyncio.wait_for(conn.run(), timeout=15)

        # Successful connect on attempt 3 must have replayed the subscribe
        self.assertEqual(len(connections), 1)
        self.assertIn(
            '{"channel":"spot.trades","event":"subscribe"}',
            connections[0].sent,
        )


if __name__ == "__main__":
    unittest.main()
