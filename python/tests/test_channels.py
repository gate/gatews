# !/usr/bin/env python
# coding: utf-8
"""
Tests for the concrete channel classes exposed in gate_ws.spot and
gate_ws.futures, and for the channel-name / require_auth invariants
that downstream user code relies on.
"""
import json
import unittest
from unittest.mock import MagicMock

from gate_ws.client import Configuration, WebSocketRequest
from gate_ws import spot, futures


SPOT_PUBLIC_CHANNELS = {
    "SpotTickerChannel": "spot.tickers",
    "SpotPublicTradeChannel": "spot.trades",
    "SpotCandlesticksChannel": "spot.candlesticks",
    "SpotBookTickerChannel": "spot.book_ticker",
    "SpotOrderBookUpdateChannel": "spot.order_book_update",
    "SpotOrderBookChannel": "spot.order_book",
}

SPOT_PRIVATE_CHANNELS = {
    "SpotOrderChannel": "spot.orders",
    "SpotUserTradesChannel": "spot.usertrades",
    "SpotBalanceChannel": "spot.balances",
    "SpotMarginBalanceChannel": "spot.margin_balances",
    "SpotFundingBalanceChannel": "spot.funding_balances",
    "SpotCrossMarginBalanceChannel": "spot.cross_balances",
}

FUTURES_PUBLIC_CHANNELS = {
    "FuturesTickerChannel": "futures.tickers",
    "FuturesPublicTradeChannel": "futures.trades",
    "FuturesCandlesticksChannel": "futures.candlesticks",
    "FuturesBookTickerChannel": "futures.book_ticker",
    "FuturesOrderBookUpdateChannel": "futures.order_book_update",
    "FuturesOrderBookChannel": "futures.order_book",
}

FUTURES_PRIVATE_CHANNELS = {
    "FuturesOrderChannel": "futures.orders",
    "FuturesUserTradesChannel": "futures.usertrades",
    "FuturesLiquidatesChannel": "futures.liquidates",
    "FuturesADLChannel": "futures.auto_deleverages",
    "FuturesPositionClosesChannel": "futures.position_closes",
    "FuturesBalanceChannel": "futures.balances",
    "FuturesReduceRiskLimitChannel": "futures.reduce_risk_limits",
    "FuturesPositionsChannel": "futures.positions",
    "FuturesAutoOrdersChannel": "futures.autoorders",
}


class _FakeConnection:
    """Minimal Connection stand-in: records send calls and exposes register/unregister."""

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


class SpotChannelNameTests(unittest.TestCase):
    def test_public_channel_names_and_no_auth(self):
        for class_name, expected_name in SPOT_PUBLIC_CHANNELS.items():
            cls = getattr(spot, class_name)
            self.assertEqual(cls.name, expected_name, f"{class_name}.name")
            self.assertFalse(cls.require_auth, f"{class_name} must be public")

    def test_private_channel_names_and_require_auth(self):
        for class_name, expected_name in SPOT_PRIVATE_CHANNELS.items():
            cls = getattr(spot, class_name)
            self.assertEqual(cls.name, expected_name, f"{class_name}.name")
            self.assertTrue(cls.require_auth, f"{class_name} must require auth")

    def test_login_channel_name(self):
        self.assertEqual(spot.SpotLoginChannel.name, "spot.login")

    def test_order_management_channel_names(self):
        order_channels = {
            "SpotOrderAmendChannel": "spot.order_amend",
            "SpotOrderCancelChannel": "spot.order_cancel",
            "SpotOrderCancelCpChannel": "spot.order_cancel_cp",
            "SpotOrderCancelIdsChannel": "spot.order_cancel_ids",
            "SpotOrderPlaceChannel": "spot.order_place",
            "SpotOrderStatusChannel": "spot.order_status",
        }
        for class_name, expected in order_channels.items():
            cls = getattr(spot, class_name)
            self.assertEqual(cls.name, expected)


class FuturesChannelNameTests(unittest.TestCase):
    def test_public_channel_names_and_no_auth(self):
        for class_name, expected_name in FUTURES_PUBLIC_CHANNELS.items():
            cls = getattr(futures, class_name)
            self.assertEqual(cls.name, expected_name, f"{class_name}.name")
            self.assertFalse(cls.require_auth, f"{class_name} must be public")

    def test_private_channel_names_and_require_auth(self):
        for class_name, expected_name in FUTURES_PRIVATE_CHANNELS.items():
            cls = getattr(futures, class_name)
            self.assertEqual(cls.name, expected_name, f"{class_name}.name")
            self.assertTrue(cls.require_auth, f"{class_name} must require auth")

    def test_login_and_order_channel_names(self):
        self.assertEqual(futures.FuturesLoginChannel.name, "futures.login")
        self.assertEqual(futures.FuturesOrderPlaceChannel.name, "futures.order_place")
        self.assertEqual(futures.FuturesOrderAmendChannel.name, "futures.order_amend")
        self.assertEqual(futures.FuturesOrderBatchPlaceChannel.name, "futures.order_batch_place")


class ChannelSubscriptionWiringTests(unittest.TestCase):
    """Verify that subscribe()/unsubscribe()/login()/api_request() on each
    concrete channel produce request payloads with the correct channel name
    and event."""

    def _setup(self, channel_class, app="spot"):
        cfg = Configuration(app=app, api_key="K", api_secret="S")
        conn = _FakeConnection(cfg)
        instance = channel_class(conn)
        return conn, instance

    def test_spot_public_subscribe_no_auth_signature(self):
        conn, channel = self._setup(spot.SpotPublicTradeChannel)
        channel.subscribe(["BTC_USDT"])
        self.assertEqual(len(conn.sent), 1)
        self.assertIsInstance(conn.sent[0], WebSocketRequest)
        data = json.loads(str(conn.sent[0]))
        self.assertEqual(data["channel"], "spot.trades")
        self.assertEqual(data["event"], "subscribe")
        self.assertEqual(data["payload"], ["BTC_USDT"])
        self.assertNotIn("auth", data)

    def test_spot_private_subscribe_includes_auth(self):
        conn, channel = self._setup(spot.SpotOrderChannel)
        channel.subscribe(["BTC_USDT"])
        data = json.loads(str(conn.sent[0]))
        self.assertEqual(data["channel"], "spot.orders")
        self.assertIn("auth", data)
        self.assertEqual(data["auth"]["KEY"], "K")
        self.assertEqual(data["auth"]["method"], "api_key")
        self.assertIn("SIGN", data["auth"])

    def test_futures_private_subscribe_includes_auth(self):
        conn, channel = self._setup(futures.FuturesOrderChannel, app="futures")
        channel.subscribe(["BTC_USDT"])
        data = json.loads(str(conn.sent[0]))
        self.assertEqual(data["channel"], "futures.orders")
        self.assertIn("auth", data)
        self.assertEqual(data["auth"]["KEY"], "K")

    def test_spot_unsubscribe_event(self):
        conn, channel = self._setup(spot.SpotTickerChannel)
        channel.unsubscribe(["BTC_USDT"])
        data = json.loads(str(conn.sent[0]))
        self.assertEqual(data["event"], "unsubscribe")

    def test_login_channel_routes_to_spot_login_for_spot_app(self):
        conn, channel = self._setup(spot.SpotOrderPlaceChannel)
        channel.login("hdr", "rid")
        # login() emits a single login request via ApiRequest; payload string contains "spot.login"
        self.assertEqual(len(conn.sent), 1)
        login_payload = json.loads(conn.sent[0])
        self.assertEqual(login_payload["channel"], "spot.login")
        self.assertEqual(login_payload["event"], "api")

    def test_login_channel_routes_to_futures_login_for_futures_app(self):
        conn, channel = self._setup(futures.FuturesOrderPlaceChannel, app="futures")
        channel.login("hdr", "rid")
        login_payload = json.loads(conn.sent[0])
        self.assertEqual(login_payload["channel"], "futures.login")

    def test_api_request_emits_login_then_request(self):
        conn, channel = self._setup(spot.SpotOrderPlaceChannel)
        channel.api_request({"currency_pair": "BTC_USDT"}, header="hdr", req_id="rid")
        # api_request() calls login() then sends the api request → 2 outbound msgs
        self.assertEqual(len(conn.sent), 2)
        login_payload = json.loads(conn.sent[0])
        api_payload = json.loads(conn.sent[1])
        self.assertEqual(login_payload["channel"], "spot.login")
        self.assertEqual(api_payload["channel"], "spot.order_place")
        self.assertEqual(api_payload["event"], "api")
        self.assertEqual(
            api_payload["payload"]["req_param"]["currency_pair"], "BTC_USDT"
        )
        self.assertEqual(api_payload["payload"]["req_id"], "rid")


class AuthChannelInvariantTests(unittest.TestCase):
    """Cross-check that channel classes flagged require_auth=True match
    the user-facing convention. Catches regressions where a private
    channel is accidentally exposed without auth."""

    def test_spot_order_management_channels_dont_require_auth_at_subscribe_level(self):
        # Order amendment/cancellation/etc are sent via api_request, not
        # subscribe, so their classes are intentionally non-auth.
        for class_name in [
            "SpotOrderAmendChannel",
            "SpotOrderCancelChannel",
            "SpotOrderCancelCpChannel",
            "SpotOrderCancelIdsChannel",
            "SpotOrderPlaceChannel",
            "SpotOrderStatusChannel",
        ]:
            cls = getattr(spot, class_name)
            self.assertFalse(
                cls.require_auth,
                f"{class_name} should be require_auth=False (auth is "
                "carried by api_request signature, not subscribe signature)",
            )

    def test_futures_order_management_channels_dont_require_auth_at_subscribe_level(self):
        for class_name in [
            "FuturesOrderAmendChannel",
            "FuturesOrderCancelChannel",
            "FuturesOrderCancelCpChannel",
            "FuturesOrderPlaceChannel",
            "FuturesOrderBatchPlaceChannel",
            "FuturesOrderStatusChannel",
            "FuturesOrderListChannel",
        ]:
            cls = getattr(futures, class_name)
            self.assertFalse(cls.require_auth, f"{class_name} require_auth")


if __name__ == "__main__":
    unittest.main()
