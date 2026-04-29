import asyncio
import hashlib
import hmac
import json
import unittest
from unittest.mock import patch

from gate_ws.client import (
    ApiRequest,
    Configuration,
    Connection,
    GateWebsocketError,
    GateWebSocketUpgrade,
    WebSocketRequest,
    WebSocketResponse,
)
from gate_ws.spot import SpotOrderChannel


class DummyConnection:
    def __init__(self, cfg):
        self.cfg = cfg
        self.sent = []
        self.channels = {}

    def send(self, msg):
        self.sent.append(msg)

    def register(self, channel, callback=None):
        if callback:
            self.channels[channel] = callback

    def unregister(self, channel):
        self.channels.pop(channel, None)


class DummyChannel(SpotOrderChannel):
    pass


class ClientTests(unittest.TestCase):
    def test_configuration_default_host_for_spot_and_futures(self):
        spot = Configuration()
        self.assertEqual(spot.host, "wss://api.gateio.ws/ws/v4/")

        futures = Configuration(app="futures", settle="btc")
        self.assertEqual(futures.host, "wss://fx-ws.gateio.ws/v4/ws/btc")

        testnet = Configuration(app="futures", settle="usdt", test_net=True)
        self.assertEqual(testnet.host, "wss://fx-ws-testnet.gateio.ws/v4/ws/usdt")

    @patch("gate_ws.client.time.time", return_value=1700000000)
    def test_websocket_request_serialization_with_auth(self, mocked_time):
        cfg = Configuration(api_key="KEY", api_secret="SECRET")
        req = WebSocketRequest(cfg, "spot.orders", "subscribe", ["BTC_USDT"], True)
        data = json.loads(str(req))

        self.assertEqual(data["time"], 1700000000)
        self.assertEqual(data["channel"], "spot.orders")
        self.assertEqual(data["event"], "subscribe")
        self.assertEqual(data["payload"], ["BTC_USDT"])

        message = "channel=%s&event=%s&time=%d" % (
            "spot.orders",
            "subscribe",
            1700000000,
        )
        expected_sign = hmac.new(
            b"SECRET",
            message.encode("utf8"),
            hashlib.sha512,
        ).hexdigest()
        self.assertEqual(data["auth"]["method"], "api_key")
        self.assertEqual(data["auth"]["KEY"], "KEY")
        self.assertEqual(data["auth"]["SIGN"], expected_sign)

    @patch("gate_ws.client.time.time", return_value=1700000000)
    def test_api_request_serialization(self, mocked_time):
        cfg = Configuration(api_key="KEY", api_secret="SECRET")
        req = ApiRequest(
            cfg,
            "spot.order_place",
            header="X-Header",
            req_id="req-1",
            payload={"currency_pair": "BTC_USDT"},
        )
        data = json.loads(req.gen())

        self.assertEqual(data["time"], 1700000000)
        self.assertEqual(data["channel"], "spot.order_place")
        self.assertEqual(data["event"], "api")
        self.assertEqual(data["payload"]["api_key"], "KEY")
        self.assertEqual(data["payload"]["req_id"], "req-1")
        self.assertEqual(data["payload"]["req_header"], {"X-Gate-Channel-Id": "X-Header"})
        self.assertEqual(data["payload"]["req_param"], {"currency_pair": "BTC_USDT"})

        message = "api\nspot.order_place\n%s\n%d" % (
            json.dumps({"currency_pair": "BTC_USDT"}),
            1700000000,
        )
        expected_sign = hmac.new(
            b"SECRET",
            message.encode("utf8"),
            hashlib.sha512,
        ).hexdigest()
        self.assertEqual(data["payload"]["signature"], expected_sign)

    def test_websocket_response_local_ts_only_for_dict(self):
        list_body = json.dumps(
            {
                "channel": "spot.trades",
                "event": "update",
                "result": [{"price": "100"}],
            }
        )
        list_response = WebSocketResponse(list_body, local_ts=123)
        self.assertEqual(list_response.result, [{"price": "100"}])

        empty_list_body = json.dumps(
            {
                "channel": "spot.trades",
                "event": "update",
                "result": [],
            }
        )
        empty_list_response = WebSocketResponse(empty_list_body, local_ts=123)
        self.assertEqual(empty_list_response.result, [])

        dict_body = json.dumps(
            {
                "header": {"channel": "spot.orders"},
                "event": "update",
                "result": {"id": "1"},
            }
        )
        dict_response = WebSocketResponse(dict_body, local_ts=123)
        self.assertEqual(dict_response.channel, "spot.orders")
        self.assertEqual(dict_response.result["_local_ts"], 123)
        self.assertEqual(dict_response.result["id"], "1")

    def test_api_request_accepts_empty_payload(self):
        cfg = Configuration(api_key="KEY", api_secret="SECRET")
        req = ApiRequest(cfg, "spot.order_place")
        data = json.loads(req.gen())

        self.assertEqual(data["payload"]["req_param"], {})
        self.assertEqual(data["payload"]["req_id"], "")

    def test_connection_uses_running_loop_when_cfg_loop_is_none(self):
        async def runner():
            conn = Connection(Configuration())
            self.assertIs(conn.event_loop, asyncio.get_running_loop())

        asyncio.run(runner())

    def test_connection_prefers_cfg_loop_when_provided(self):
        loop = asyncio.new_event_loop()
        try:
            conn = Connection(Configuration(event_loop=loop))
            self.assertIs(conn.event_loop, loop)
        finally:
            loop.close()

    def test_base_channel_subscription_and_api_request_paths(self):
        cfg = Configuration(app="spot", api_key="KEY", api_secret="SECRET")
        conn = DummyConnection(cfg)
        channel = DummyChannel(conn)

        channel.subscribe(["BTC_USDT"])
        self.assertEqual(len(conn.sent), 1)
        self.assertIsInstance(conn.sent[0], WebSocketRequest)
        self.assertEqual(conn.sent[0].channel, "spot.orders")
        self.assertEqual(conn.sent[0].event, "subscribe")

        channel.unsubscribe(["BTC_USDT"])
        self.assertEqual(len(conn.sent), 2)
        self.assertEqual(conn.sent[1].event, "unsubscribe")

        channel.login("X-Header", "req-1")
        self.assertEqual(len(conn.sent), 3)
        login_payload = json.loads(conn.sent[2])
        self.assertEqual(login_payload["channel"], "spot.login")
        self.assertEqual(login_payload["event"], "api")

        channel.api_request({"currency_pair": "BTC_USDT"}, header="X-Header", req_id="req-2")
        self.assertEqual(len(conn.sent), 5)
        api_login_payload = json.loads(conn.sent[3])
        api_request_payload = json.loads(conn.sent[4])
        self.assertEqual(api_login_payload["channel"], "spot.login")
        self.assertEqual(api_request_payload["channel"], "spot.orders")
        self.assertEqual(api_request_payload["event"], "api")

    def test_gate_websocket_upgrade_message(self):
        exc = GateWebSocketUpgrade()
        self.assertEqual(str(exc), "Server requests connection upgrade")


class ConfigurationEdgeCaseTests(unittest.TestCase):
    def test_configuration_with_custom_host_overrides_default(self):
        cfg = Configuration(host="wss://custom.example/ws/v4/")
        self.assertEqual(cfg.host, "wss://custom.example/ws/v4/")

    def test_configuration_passes_through_optional_params(self):
        loop = asyncio.new_event_loop()
        try:
            cfg = Configuration(
                event_loop=loop,
                ping_interval=30,
                max_retry=42,
                verify=False,
                add_local_ts=True,
                api_key="K",
                api_secret="S",
            )
            self.assertIs(cfg.loop, loop)
            self.assertEqual(cfg.ping_interval, 30)
            self.assertEqual(cfg.max_retry, 42)
            self.assertFalse(cfg.verify)
            self.assertTrue(cfg.add_local_ts)
            self.assertEqual(cfg.api_key, "K")
            self.assertEqual(cfg.api_secret, "S")
        finally:
            loop.close()


class WebSocketRequestEdgeCaseTests(unittest.TestCase):
    def test_request_without_auth_omits_signature(self):
        cfg = Configuration()
        req = WebSocketRequest(cfg, "spot.trades", "subscribe", ["BTC_USDT"], False)
        data = json.loads(str(req))
        self.assertNotIn("auth", data)
        self.assertEqual(data["channel"], "spot.trades")
        self.assertEqual(data["payload"], ["BTC_USDT"])

    def test_request_with_auth_but_missing_credentials_raises(self):
        cfg = Configuration()
        req = WebSocketRequest(cfg, "spot.orders", "subscribe", ["BTC_USDT"], True)
        with self.assertRaises(ValueError):
            str(req)


class ApiRequestEdgeCaseTests(unittest.TestCase):
    def test_api_request_missing_credentials_raises(self):
        cfg = Configuration()
        with self.assertRaises(ValueError):
            ApiRequest(cfg, "spot.order_place")

    def test_api_request_omits_user_payload_in_signature_when_empty(self):
        cfg = Configuration(api_key="KEY", api_secret="SECRET")
        req = ApiRequest(cfg, "spot.order_place", req_id="rid")
        data = json.loads(req.gen())
        self.assertEqual(data["payload"]["req_param"], {})
        self.assertEqual(data["payload"]["req_id"], "rid")


class WebSocketResponseEdgeCaseTests(unittest.TestCase):
    def test_response_without_channel_raises(self):
        body = json.dumps({"event": "update", "result": {}})
        with self.assertRaises(ValueError):
            WebSocketResponse(body)

    def test_response_with_error_populates_error_field(self):
        body = json.dumps(
            {
                "channel": "spot.orders",
                "event": "update",
                "error": {"code": 4001, "message": "auth failed"},
            }
        )
        resp = WebSocketResponse(body)
        self.assertIsNotNone(resp.error)
        self.assertEqual(resp.error.code, 4001)
        self.assertEqual(resp.error.message, "auth failed")

    def test_response_uses_header_channel_when_top_level_missing(self):
        body = json.dumps(
            {
                "header": {"channel": "spot.order_place"},
                "event": "api",
                "result": {"id": "1"},
            }
        )
        resp = WebSocketResponse(body)
        self.assertEqual(resp.channel, "spot.order_place")
        self.assertEqual(resp.result["id"], "1")

    def test_response_exposes_event_and_timestamp(self):
        body = json.dumps(
            {
                "channel": "spot.trades",
                "event": "update",
                "time": 1700000099,
                "result": [{"price": "100"}],
            }
        )
        resp = WebSocketResponse(body)
        self.assertEqual(resp.event, "update")
        self.assertEqual(resp.timestamp, 1700000099)

    def test_response_falls_back_to_data_result(self):
        body = json.dumps(
            {
                "channel": "spot.order_place",
                "event": "api",
                "data": {"result": {"order_id": "abc"}},
            }
        )
        resp = WebSocketResponse(body)
        self.assertEqual(resp.result, {"order_id": "abc"})

    def test_response_falls_back_to_data_errs_when_no_result(self):
        body = json.dumps(
            {
                "channel": "spot.order_place",
                "event": "api",
                "data": {"errs": {"label": "INVALID", "message": "bad"}},
            }
        )
        resp = WebSocketResponse(body)
        self.assertEqual(resp.result["label"], "INVALID")

    def test_response_skips_local_ts_injection_when_disabled(self):
        body = json.dumps({"channel": "spot.orders", "event": "update", "result": {"id": "1"}})
        resp = WebSocketResponse(body)
        self.assertNotIn("_local_ts", resp.result)
        self.assertIsNone(resp.local_ts)

    def test_response_skips_local_ts_when_value_is_zero(self):
        body = json.dumps({"channel": "spot.orders", "event": "update", "result": {"id": "1"}})
        resp = WebSocketResponse(body, local_ts=0)
        self.assertNotIn("_local_ts", resp.result)


class ConnectionLifecycleTests(unittest.TestCase):
    def test_send_enqueues_message(self):
        async def runner():
            conn = Connection(Configuration())
            conn.send("hello")
            self.assertEqual(conn.sending_queue.qsize(), 1)
            self.assertEqual(conn.sending_queue.get_nowait(), "hello")

        asyncio.run(runner())

    def test_register_and_unregister_callback(self):
        conn = Connection(Configuration(event_loop=asyncio.new_event_loop()))
        try:
            cb = lambda c, r: None
            conn.register("spot.trades", cb)
            self.assertIs(conn.channels["spot.trades"], cb)

            conn.register("spot.trades", None)
            # registering with None should leave existing callback intact
            self.assertIs(conn.channels["spot.trades"], cb)

            conn.unregister("spot.trades")
            self.assertNotIn("spot.trades", conn.channels)

            # unregister of unknown channel is a no-op
            conn.unregister("missing.channel")
        finally:
            conn.event_loop.close()

    def test_close_cancels_main_loop(self):
        from unittest.mock import MagicMock

        conn = Connection(Configuration(event_loop=asyncio.new_event_loop()))
        try:
            mock_main = MagicMock()
            conn.main_loop = mock_main
            conn.close()
            mock_main.cancel.assert_called_once()
        finally:
            conn.event_loop.close()

    def test_close_when_main_loop_is_none_is_noop(self):
        conn = Connection(Configuration(event_loop=asyncio.new_event_loop()))
        try:
            conn.close()  # should not raise
            self.assertIsNone(conn.main_loop)
        finally:
            conn.event_loop.close()


class GateWebsocketErrorTests(unittest.TestCase):
    """Coverage for the GateWebsocketError exception class itself."""

    def test_init_stores_code_and_message(self):
        err = GateWebsocketError(4001, "auth failed")
        self.assertEqual(err.code, 4001)
        self.assertEqual(err.message, "auth failed")

    def test_str_renders_code_and_message(self):
        err = GateWebsocketError(500, "server error")
        self.assertEqual(str(err), "code: 500, message: server error")

    def test_is_exception_subclass(self):
        err = GateWebsocketError(0, "")
        self.assertIsInstance(err, Exception)

    def test_can_be_raised_and_caught(self):
        with self.assertRaises(GateWebsocketError) as ctx:
            raise GateWebsocketError(123, "boom")
        self.assertEqual(ctx.exception.code, 123)


class ConfigurationFuturesHostTests(unittest.TestCase):
    """Detailed host derivation tests across the (app, settle, test_net)
    grid. Existing tests cover the basic spot/usdt path; this class covers
    the BTC settlement and testnet permutations."""

    def test_futures_btc_default_host(self):
        cfg = Configuration(app="futures", settle="btc")
        self.assertEqual(cfg.host, "wss://fx-ws.gateio.ws/v4/ws/btc")

    def test_futures_usdt_default_host(self):
        cfg = Configuration(app="futures", settle="usdt")
        self.assertEqual(cfg.host, "wss://fx-ws.gateio.ws/v4/ws/usdt")

    def test_futures_btc_testnet_host(self):
        cfg = Configuration(app="futures", settle="btc", test_net=True)
        self.assertEqual(cfg.host, "wss://fx-ws-testnet.gateio.ws/v4/ws/btc")

    def test_futures_usdt_testnet_host(self):
        cfg = Configuration(app="futures", settle="usdt", test_net=True)
        self.assertEqual(cfg.host, "wss://fx-ws-testnet.gateio.ws/v4/ws/usdt")

    def test_spot_test_net_does_not_change_host(self):
        # test_net only affects futures host derivation
        cfg = Configuration(test_net=True)
        self.assertEqual(cfg.host, "wss://api.gateio.ws/ws/v4/")

    def test_explicit_host_overrides_settle_and_testnet(self):
        cfg = Configuration(
            app="futures",
            settle="usdt",
            test_net=True,
            host="wss://overridden.example.com/ws/",
        )
        self.assertEqual(cfg.host, "wss://overridden.example.com/ws/")


class ConfigurationDocstringInvariantTests(unittest.TestCase):
    """Verify defaults documented in the Configuration docstring."""

    def test_default_ping_interval_is_5_seconds(self):
        # Per docstring: "Active ping interval to websocket server, default to 5 seconds"
        cfg = Configuration()
        self.assertEqual(cfg.ping_interval, 5)

    def test_default_max_retry_is_10(self):
        # Per docstring: "Reconnect will be given up if max_retry reached. Default to 10."
        cfg = Configuration()
        self.assertEqual(cfg.max_retry, 10)

    def test_default_verify_is_true(self):
        # Per docstring: "enable certificate verification, default to True"
        cfg = Configuration()
        self.assertTrue(cfg.verify)

    def test_default_app_is_spot(self):
        cfg = Configuration()
        self.assertEqual(cfg.app, "spot")

    def test_default_test_net_is_false_implicit(self):
        # No test_net=True → futures host does NOT include "testnet"
        cfg = Configuration(app="futures")
        self.assertNotIn("testnet", cfg.host)

    def test_default_add_local_ts_is_false(self):
        cfg = Configuration()
        self.assertFalse(cfg.add_local_ts)

    def test_default_loop_pool_callback_are_none(self):
        cfg = Configuration()
        self.assertIsNone(cfg.loop)
        self.assertIsNone(cfg.pool)
        self.assertIsNone(cfg.default_callback)


class WebSocketRequestEdgeCaseExtraTests(unittest.TestCase):
    def test_request_payload_dict_serializes_intact(self):
        cfg = Configuration()
        payload = {"currency_pair": "BTC_USDT", "interval": "1m"}
        req = WebSocketRequest(cfg, "spot.candlesticks", "subscribe", payload, False)
        data = json.loads(str(req))
        self.assertEqual(data["payload"], payload)

    def test_request_includes_time_field(self):
        cfg = Configuration()
        req = WebSocketRequest(cfg, "spot.trades", "subscribe", [], False)
        data = json.loads(str(req))
        self.assertIn("time", data)
        self.assertIsInstance(data["time"], int)

    def test_request_attributes_persist(self):
        cfg = Configuration()
        req = WebSocketRequest(cfg, "spot.trades", "subscribe", ["BTC_USDT"], False)
        self.assertEqual(req.channel, "spot.trades")
        self.assertEqual(req.event, "subscribe")
        self.assertEqual(req.payload, ["BTC_USDT"])
        self.assertFalse(req.require_auth)
        self.assertIs(req.cfg, cfg)


if __name__ == "__main__":
    unittest.main()
