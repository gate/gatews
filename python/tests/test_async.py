# !/usr/bin/env python
# coding: utf-8
"""
End-to-end tests for the asynchronous parts of gate_ws.client:
- _read coroutine: callback dispatch (sync/async/default), upgrade event detection
- _write coroutine: history replay + queue draining
- _active_ping coroutine: periodic ping frames
- Connection.run loop: max_retry behavior on dial failure
"""
import asyncio
import json
import unittest
from unittest.mock import patch

from gate_ws.client import (
    Configuration,
    Connection,
    GateWebSocketUpgrade,
)


class FakeWebSocket:
    """Stand-in for the object returned by websockets.connect.

    Supports the subset of the API used by Connection: send(), close(), and
    async iteration (async for msg in conn:). Iteration terminates cleanly
    via StopAsyncIteration when the incoming buffer is exhausted.
    """

    def __init__(self, incoming=None):
        self._incoming = list(incoming or [])
        self.sent = []
        self.closed = False

    async def send(self, msg):
        self.sent.append(msg)

    async def close(self):
        self.closed = True

    def __aiter__(self):
        return self

    async def __anext__(self):
        # Yield to scheduler so other coroutines can interleave
        await asyncio.sleep(0)
        if not self._incoming:
            raise StopAsyncIteration
        msg = self._incoming.pop(0)
        if isinstance(msg, BaseException):
            raise msg
        return msg


def _trade_msg(channel="spot.trades", event="update", result=None):
    body = {"channel": channel, "event": event}
    body["result"] = result if result is not None else [{"price": "100"}]
    return json.dumps(body)


class ReadCoroutineTests(unittest.IsolatedAsyncioTestCase):
    async def _build_connection(self, cfg=None):
        cfg = cfg or Configuration()
        conn = Connection(cfg)
        conn.event_loop = asyncio.get_running_loop()
        return conn

    async def test_dispatches_async_callback(self):
        conn = await self._build_connection()
        received = []

        async def cb(c, response):
            received.append(response.channel)

        conn.register("spot.trades", cb)
        await conn._read(FakeWebSocket([_trade_msg()]))
        await asyncio.sleep(0.01)  # let scheduled callback task complete
        self.assertEqual(received, ["spot.trades"])

    async def test_dispatches_sync_callback_via_executor(self):
        conn = await self._build_connection()
        received = []

        def cb(c, response):
            received.append(response.channel)

        conn.register("spot.trades", cb)
        await conn._read(FakeWebSocket([_trade_msg()]))
        # Sync callback runs in default executor; allow it to finish
        await asyncio.sleep(0.05)
        self.assertEqual(received, ["spot.trades"])

    async def test_default_callback_for_unknown_channel(self):
        received = []

        async def default_cb(c, response):
            received.append(("default", response.channel))

        conn = await self._build_connection(Configuration(default_callback=default_cb))
        # No channel-specific callback registered
        await conn._read(FakeWebSocket([_trade_msg(channel="spot.unknown")]))
        await asyncio.sleep(0.01)
        self.assertEqual(received, [("default", "spot.unknown")])

    async def test_no_callback_silently_consumes_message(self):
        conn = await self._build_connection()
        # No callback, no default; iteration should complete without raising
        await conn._read(FakeWebSocket([_trade_msg()]))

    async def test_upgrade_event_raises_gate_websocket_upgrade(self):
        conn = await self._build_connection()
        body = json.dumps({"channel": "spot.trades", "event": "upgrade", "result": {}})
        with self.assertRaises(GateWebSocketUpgrade):
            await conn._read(FakeWebSocket([body]))

    async def test_local_ts_injected_when_enabled(self):
        conn = await self._build_connection(Configuration(add_local_ts=True))
        captured = []

        async def cb(c, response):
            captured.append(response.result)

        conn.register("spot.orders", cb)
        body = json.dumps({"channel": "spot.orders", "event": "update", "result": {"id": "1"}})
        await conn._read(FakeWebSocket([body]))
        await asyncio.sleep(0.01)
        self.assertEqual(len(captured), 1)
        self.assertIn("_local_ts", captured[0])
        self.assertGreater(captured[0]["_local_ts"], 0)

    async def test_local_ts_skipped_for_list_result_when_enabled(self):
        # Regression test: list-result must not crash with TypeError under add_local_ts=True
        conn = await self._build_connection(Configuration(add_local_ts=True))
        captured = []

        async def cb(c, response):
            captured.append(response.result)

        conn.register("spot.trades", cb)
        body = json.dumps({"channel": "spot.trades", "event": "update", "result": [{"price": "100"}]})
        await conn._read(FakeWebSocket([body]))
        await asyncio.sleep(0.01)
        self.assertEqual(captured, [[{"price": "100"}]])  # untouched list


class ActivePingTests(unittest.IsolatedAsyncioTestCase):
    async def test_periodic_ping_sent_with_correct_channel(self):
        cfg = Configuration(ping_interval=0.01)
        conn = Connection(cfg)
        conn.event_loop = asyncio.get_running_loop()

        fake = FakeWebSocket()
        ping_task = asyncio.create_task(conn._active_ping(fake))
        try:
            await asyncio.sleep(0.05)
        finally:
            ping_task.cancel()
            try:
                await ping_task
            except asyncio.CancelledError:
                pass

        self.assertGreaterEqual(len(fake.sent), 2)
        for raw in fake.sent:
            data = json.loads(raw)
            self.assertEqual(data["channel"], "spot.ping")
            self.assertIn("time", data)

    async def test_active_ping_uses_app_for_futures(self):
        cfg = Configuration(app="futures", ping_interval=0.01)
        conn = Connection(cfg)
        conn.event_loop = asyncio.get_running_loop()

        fake = FakeWebSocket()
        ping_task = asyncio.create_task(conn._active_ping(fake))
        try:
            await asyncio.sleep(0.03)
        finally:
            ping_task.cancel()
            try:
                await ping_task
            except asyncio.CancelledError:
                pass

        self.assertGreaterEqual(len(fake.sent), 1)
        data = json.loads(fake.sent[0])
        self.assertEqual(data["channel"], "futures.ping")


class WriteCoroutineTests(unittest.IsolatedAsyncioTestCase):
    async def test_replays_history_then_drains_queue(self):
        cfg = Configuration()
        conn = Connection(cfg)
        conn.event_loop = asyncio.get_running_loop()

        # Pre-populate history (simulating prior subscriptions before reconnect)
        conn.sending_history.append('{"channel":"history-1"}')
        conn.sending_history.append('{"channel":"history-2"}')

        fake = FakeWebSocket()
        write_task = asyncio.create_task(conn._write(fake))
        # Add new message via send queue
        conn.send('{"channel":"new-1"}')
        try:
            await asyncio.sleep(0.05)
        finally:
            write_task.cancel()
            try:
                await write_task
            except asyncio.CancelledError:
                pass

        self.assertEqual(fake.sent[0], '{"channel":"history-1"}')
        self.assertEqual(fake.sent[1], '{"channel":"history-2"}')
        self.assertEqual(fake.sent[2], '{"channel":"new-1"}')
        # New message must also be appended to history for future reconnect replay
        self.assertIn('{"channel":"new-1"}', conn.sending_history)


class RunLoopTests(unittest.IsolatedAsyncioTestCase):
    async def test_run_gives_up_after_max_retry_on_dial_failure(self):
        # max_retry=1 → 2 attempts; back-off totals 0.5s. Keep timeout generous.
        cfg = Configuration(max_retry=1)
        conn = Connection(cfg)

        attempts = 0

        async def failing_connect(*_args, **_kwargs):
            nonlocal attempts
            attempts += 1
            raise OSError("connection refused")

        with patch("gate_ws.client.websockets.connect", failing_connect):
            await asyncio.wait_for(conn.run(), timeout=10)

        # max_retry=1 means: 1 initial + 1 retry = 2 attempts before giving up
        self.assertEqual(attempts, 2)

    async def test_run_unexpected_exception_triggers_reconnect(self):
        """A non-ConnectionClosed runtime error inside the gather block must not
        terminate run(); it should fall through to except Exception and retry."""
        cfg = Configuration(max_retry=1)
        conn = Connection(cfg)

        connect_calls = 0

        async def failing_connect(*_args, **_kwargs):
            nonlocal connect_calls
            connect_calls += 1
            if connect_calls == 1:
                # First attempt: succeed but then _read will explode (TypeError, etc.)
                return _ExplodingWebSocket()
            # Subsequent attempts: fail with OSError to terminate retry loop quickly
            raise OSError("done")

        with patch("gate_ws.client.websockets.connect", failing_connect):
            await asyncio.wait_for(conn.run(), timeout=10)

        # Must have attempted at least 2 connections: the exploding one + retries that fail
        self.assertGreaterEqual(connect_calls, 2)


class _ExplodingWebSocket(FakeWebSocket):
    """A FakeWebSocket whose iteration raises an unexpected RuntimeError.
    Used to verify that run()'s bottom-catch reconnects on arbitrary errors."""

    async def __anext__(self):
        await asyncio.sleep(0)
        raise RuntimeError("synthetic runtime error")


if __name__ == "__main__":
    unittest.main()
