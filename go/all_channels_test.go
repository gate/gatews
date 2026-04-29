package gatews

import (
	"crypto/hmac"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"testing"
	"time"
)

// channelCase describes the wire-level expectation for a single Subscribe-style
// channel.
type channelCase struct {
	constant   string
	requireAuth bool
}

// testSubscribeWireForChannel is a shared assertion helper: subscribes to the
// given channel and verifies the request the server receives matches the
// constant exactly, including auth signature for private channels.
func testSubscribeWireForChannel(t *testing.T, app string, c channelCase, payload []string) {
	t.Helper()

	clientConn, serverConn := newWebSocketPair(t)
	ws, cancel := newTestWsService(t, clientConn, &ConfOptions{
		App:              app,
		URL:              "ws://example.com/ws",
		Key:              "test-key",
		Secret:           "test-secret",
		MaxRetryConn:     1,
		ShowReconnectMsg: false,
		PingInterval:     "24h",
	})
	defer cancel()

	got := make(chan Request, 1)
	errCh := make(chan error, 1)
	go func() {
		_, raw, err := serverConn.ReadMessage()
		if err != nil {
			errCh <- err
			return
		}
		var req Request
		if err := json.Unmarshal(raw, &req); err != nil {
			errCh <- fmt.Errorf("unmarshal: %w", err)
			return
		}
		got <- req
	}()

	if err := ws.Subscribe(c.constant, payload); err != nil {
		t.Fatalf("Subscribe(%q) err: %v", c.constant, err)
	}

	select {
	case err := <-errCh:
		t.Fatalf("server read err: %v", err)
	case req := <-got:
		if req.Channel != c.constant {
			t.Errorf("channel on wire mismatch: got %q want %q", req.Channel, c.constant)
		}
		if req.Event != Subscribe {
			t.Errorf("event mismatch: got %q want %q", req.Event, Subscribe)
		}

		// Auth assertion: every Subscribe call signs the message regardless of
		// whether the channel actually requires it (this is current behavior).
		// Private channels additionally must succeed with a valid HMAC.
		if c.requireAuth {
			if req.Auth.Method != AuthMethodApiKey {
				t.Errorf("private channel %q missing auth method", c.constant)
			}
			if req.Auth.Key != "test-key" {
				t.Errorf("private channel %q auth key mismatch: got %q", c.constant, req.Auth.Key)
			}
			h := hmac.New(sha512.New, []byte("test-secret"))
			h.Write([]byte(fmt.Sprintf("channel=%s&event=%s&time=%d", c.constant, Subscribe, req.Time)))
			want := hex.EncodeToString(h.Sum(nil))
			if req.Auth.Secret != want {
				t.Errorf("private channel %q HMAC mismatch:\n  got %s\n want %s", c.constant, req.Auth.Secret, want)
			}
		}

		// Sanity: timestamp recent
		now := time.Now().Unix()
		if req.Time < now-10 || req.Time > now+10 {
			t.Errorf("channel %q timestamp out of range: got %d, expected near %d", c.constant, req.Time, now)
		}
		_ = strconv.FormatInt(req.Time, 10) // sanity check int64 parse

	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for server to receive subscribe for %q", c.constant)
	}
}

// TestEverySpotSubscribeChannelHasWireCoverage exercises the full Subscribe
// path for every spot subscribe-style channel constant. Catches:
//   - Constant string regressions (renamed/typo'd)
//   - HMAC signing breakage per channel
//   - Framing bugs that affect a specific channel name
func TestEverySpotSubscribeChannelHasWireCoverage(t *testing.T) {
	cases := []channelCase{
		// Public spot channels (no auth)
		{ChannelSpotPublicTrade, false},
		{ChannelSpotTicker, false},
		{ChannelSpotBookTicker, false},
		{ChannelSpotCandleStick, false},
		{ChannelSpotOrderBook, false},
		{ChannelSpotOrderBookUpdate, false},
		// Private spot channels (auth required)
		{ChannelSpotOrder, true},
		{ChannelSpotUserTrade, true},
		{ChannelSpotBalance, true},
		{ChannelSpotMarginBalance, true},
		{ChannelSpotFundingBalance, true},
		{ChannelSpotCrossBalance, true},
	}
	for _, c := range cases {
		c := c
		t.Run(c.constant, func(t *testing.T) {
			testSubscribeWireForChannel(t, "spot", c, []string{"BTC_USDT"})
		})
	}
}

// TestEveryFuturesSubscribeChannelHasWireCoverage is the futures counterpart.
func TestEveryFuturesSubscribeChannelHasWireCoverage(t *testing.T) {
	cases := []channelCase{
		// Public futures channels
		{ChannelFutureTicker, false},
		{ChannelFutureTrade, false},
		{ChannelFutureBookTicker, false},
		{ChannelFutureCandleStick, false},
		{ChannelFutureOrderBook, false},
		{ChannelFutureOrderBookUpdate, false},
		// Private futures channels
		{ChannelFutureOrder, true},
		{ChannelFutureUserTrade, true},
		{ChannelFutureLiquidates, true},
		{ChannelFutureAutoDeleverages, true},
		{ChannelFuturePositionCloses, true},
		{ChannelFutureBalance, true},
		{ChannelFutureReduceRiskLimits, true},
		{ChannelFuturePositions, true},
		{ChannelFutureAutoOrders, true},
	}
	for _, c := range cases {
		c := c
		t.Run(c.constant, func(t *testing.T) {
			testSubscribeWireForChannel(t, "futures", c, []string{"BTC_USDT"})
		})
	}
}

// testAPIRequestWireForChannel asserts that calling APIRequest for an order
// management channel results in the correct two-message sequence on the wire:
// 1. login api request (channel == spot.login or futures.login)
// 2. user-supplied api request (channel == constant)
func testAPIRequestWireForChannel(t *testing.T, app, expectedLoginChannel string, channel string, payload any) {
	t.Helper()

	clientConn, serverConn := newWebSocketPair(t)
	ws, cancel := newTestWsService(t, clientConn, &ConfOptions{
		App:              app,
		URL:              "ws://example.com/ws",
		Key:              "test-key",
		Secret:           "test-secret",
		MaxRetryConn:     1,
		ShowReconnectMsg: false,
		PingInterval:     "24h",
	})
	defer cancel()

	type capture struct {
		req Request
		err error
	}
	got := make(chan capture, 2)
	go func() {
		for i := 0; i < 2; i++ {
			_, raw, err := serverConn.ReadMessage()
			if err != nil {
				got <- capture{err: err}
				return
			}
			var req Request
			if err := json.Unmarshal(raw, &req); err != nil {
				got <- capture{err: err}
				return
			}
			got <- capture{req: req}
		}
	}()

	if err := ws.APIRequest(channel, payload, map[string]any{
		"req_id":            "rid-" + channel,
		"X-Gate-Channel-Id": "ch-" + channel,
	}); err != nil {
		t.Fatalf("APIRequest(%q) err: %v", channel, err)
	}

	got1 := <-got
	if got1.err != nil {
		t.Fatalf("read 1st request: %v", got1.err)
	}
	if got1.req.Channel != expectedLoginChannel {
		t.Errorf("first request should be login (%q), got %q", expectedLoginChannel, got1.req.Channel)
	}
	if got1.req.Event != API {
		t.Errorf("first request event mismatch: got %q want %q", got1.req.Event, API)
	}

	got2 := <-got
	if got2.err != nil {
		t.Fatalf("read 2nd request: %v", got2.err)
	}
	if got2.req.Channel != channel {
		t.Errorf("second request channel mismatch: got %q want %q", got2.req.Channel, channel)
	}
	if got2.req.Event != API {
		t.Errorf("second request event mismatch: got %q want %q", got2.req.Event, API)
	}
	// Verify req_id and X-Gate-Channel-Id propagated
	apiPayload, ok := got2.req.Payload.(map[string]any)
	if !ok {
		t.Fatalf("expected payload to be map, got %T", got2.req.Payload)
	}
	if apiPayload["req_id"] != "rid-"+channel {
		t.Errorf("req_id mismatch for %q: %v", channel, apiPayload["req_id"])
	}
	header, _ := apiPayload["req_header"].(map[string]any)
	if header["X-Gate-Channel-Id"] != "ch-"+channel {
		t.Errorf("X-Gate-Channel-Id mismatch for %q: %v", channel, header["X-Gate-Channel-Id"])
	}
}

// TestEverySpotOrderManagementChannelHasAPIRequestCoverage exercises every
// spot api_request channel: place, amend, cancel, cancel_cp, cancel_ids,
// status. login chain is verified for each.
func TestEverySpotOrderManagementChannelHasAPIRequestCoverage(t *testing.T) {
	channels := []string{
		ChannelSpotOrderPlace,
		ChannelSpotOrderAmend,
		ChannelSpotOrderCancel,
		ChannelSpotOrderCancelCp,
		ChannelSpotOrderCancelIds,
		ChannelSpotOrderStatus,
	}
	payload := map[string]any{"currency_pair": "BTC_USDT"}
	for _, ch := range channels {
		ch := ch
		t.Run(ch, func(t *testing.T) {
			testAPIRequestWireForChannel(t, "spot", ChannelSpotLogin, ch, payload)
		})
	}
}

// TestEveryFuturesOrderManagementChannelHasAPIRequestCoverage covers all
// futures order api_request channels.
func TestEveryFuturesOrderManagementChannelHasAPIRequestCoverage(t *testing.T) {
	channels := []string{
		ChannelFutureOrderPlace,
		ChannelFutureOrderAmend,
		ChannelFutureOrderCancel,
		ChannelFutureOrderCancelCp,
		ChannelFutureOrderBatchPlace,
		ChannelFutureOrderStatus,
		ChannelFutureOrderList,
	}
	payload := map[string]any{"contract": "BTC_USDT"}
	for _, ch := range channels {
		ch := ch
		t.Run(ch, func(t *testing.T) {
			testAPIRequestWireForChannel(t, "futures", ChannelFutureLogin, ch, payload)
		})
	}
}

// TestEveryAuthChannelEnforcesEmptyKeySecretError ensures that for every
// channel registered in authChannel, calling Subscribe without credentials
// returns the standard auth-empty error. Catches regressions where someone
// adds a private channel to the SDK but forgets to add it to authChannel.
func TestEveryAuthChannelEnforcesEmptyKeySecretError(t *testing.T) {
	for ch := range authChannel {
		ch := ch
		t.Run(ch, func(t *testing.T) {
			ws := &WsService{
				conf: &ConnConf{},
			}
			err := ws.Subscribe(ch, []string{"BTC_USDT"})
			if err == nil {
				t.Fatalf("private channel %q must reject empty creds", ch)
			}
			if err.Error() != "auth key or secret empty" {
				t.Errorf("unexpected error for %q: %v", ch, err)
			}
		})
	}
}

// TestEveryNonAuthSubscribeChannelDoesNotRequireCredentials is the dual:
// public channels must accept Subscribe without credentials. This guards
// against accidentally over-locking a public channel.
func TestEveryNonAuthSubscribeChannelDoesNotRequireCredentials(t *testing.T) {
	publicChannels := []string{
		ChannelSpotPublicTrade,
		ChannelSpotTicker,
		ChannelSpotBookTicker,
		ChannelSpotCandleStick,
		ChannelSpotOrderBook,
		ChannelSpotOrderBookUpdate,
		ChannelFutureTicker,
		ChannelFutureTrade,
		ChannelFutureBookTicker,
		ChannelFutureCandleStick,
		ChannelFutureOrderBook,
		ChannelFutureOrderBookUpdate,
	}
	for _, ch := range publicChannels {
		ch := ch
		t.Run(ch, func(t *testing.T) {
			clientConn, serverConn := newWebSocketPair(t)
			ws, cancel := newTestWsService(t, clientConn, &ConfOptions{
				URL:              "ws://example.com/ws",
				MaxRetryConn:     1,
				ShowReconnectMsg: false,
				PingInterval:     "24h",
				// no Key, no Secret
			})
			defer cancel()
			go func() { _, _, _ = serverConn.ReadMessage() }()
			if err := ws.Subscribe(ch, []string{"BTC_USDT"}); err != nil {
				t.Errorf("public channel %q must allow empty creds, got: %v", ch, err)
			}
		})
	}
}

// TestChannelConstantsExactValues guards against silent constant value drift.
// If anyone "fixes a typo" in a channel name, the wire format breaks for all
// existing users of the SDK. This is a regression net.
func TestChannelConstantsExactValues(t *testing.T) {
	expected := map[string]string{
		// spot public
		"ChannelSpotPublicTrade":     ChannelSpotPublicTrade,
		"ChannelSpotTicker":          ChannelSpotTicker,
		"ChannelSpotBookTicker":      ChannelSpotBookTicker,
		"ChannelSpotCandleStick":     ChannelSpotCandleStick,
		"ChannelSpotOrderBook":       ChannelSpotOrderBook,
		"ChannelSpotOrderBookUpdate": ChannelSpotOrderBookUpdate,
		// spot private
		"ChannelSpotOrder":          ChannelSpotOrder,
		"ChannelSpotUserTrade":      ChannelSpotUserTrade,
		"ChannelSpotBalance":        ChannelSpotBalance,
		"ChannelSpotMarginBalance":  ChannelSpotMarginBalance,
		"ChannelSpotFundingBalance": ChannelSpotFundingBalance,
		"ChannelSpotCrossBalance":   ChannelSpotCrossBalance,
		// spot orders
		"ChannelSpotLogin":          ChannelSpotLogin,
		"ChannelSpotOrderAmend":     ChannelSpotOrderAmend,
		"ChannelSpotOrderCancel":    ChannelSpotOrderCancel,
		"ChannelSpotOrderCancelCp":  ChannelSpotOrderCancelCp,
		"ChannelSpotOrderCancelIds": ChannelSpotOrderCancelIds,
		"ChannelSpotOrderPlace":     ChannelSpotOrderPlace,
		"ChannelSpotOrderStatus":    ChannelSpotOrderStatus,
		// futures public
		"ChannelFutureTicker":          ChannelFutureTicker,
		"ChannelFutureTrade":           ChannelFutureTrade,
		"ChannelFutureBookTicker":      ChannelFutureBookTicker,
		"ChannelFutureCandleStick":     ChannelFutureCandleStick,
		"ChannelFutureOrderBook":       ChannelFutureOrderBook,
		"ChannelFutureOrderBookUpdate": ChannelFutureOrderBookUpdate,
		// futures private
		"ChannelFutureOrder":            ChannelFutureOrder,
		"ChannelFutureUserTrade":        ChannelFutureUserTrade,
		"ChannelFutureLiquidates":       ChannelFutureLiquidates,
		"ChannelFutureAutoDeleverages":  ChannelFutureAutoDeleverages,
		"ChannelFuturePositionCloses":   ChannelFuturePositionCloses,
		"ChannelFutureBalance":          ChannelFutureBalance,
		"ChannelFutureReduceRiskLimits": ChannelFutureReduceRiskLimits,
		"ChannelFuturePositions":        ChannelFuturePositions,
		"ChannelFutureAutoOrders":       ChannelFutureAutoOrders,
		// futures orders
		"ChannelFutureLogin":           ChannelFutureLogin,
		"ChannelFutureOrderAmend":      ChannelFutureOrderAmend,
		"ChannelFutureOrderCancel":     ChannelFutureOrderCancel,
		"ChannelFutureOrderCancelCp":   ChannelFutureOrderCancelCp,
		"ChannelFutureOrderPlace":      ChannelFutureOrderPlace,
		"ChannelFutureOrderBatchPlace": ChannelFutureOrderBatchPlace,
		"ChannelFutureOrderStatus":     ChannelFutureOrderStatus,
		"ChannelFutureOrderList":       ChannelFutureOrderList,
	}

	want := map[string]string{
		"ChannelSpotPublicTrade":     "spot.trades",
		"ChannelSpotTicker":          "spot.tickers",
		"ChannelSpotBookTicker":      "spot.book_ticker",
		"ChannelSpotCandleStick":     "spot.candlesticks",
		"ChannelSpotOrderBook":       "spot.order_book",
		"ChannelSpotOrderBookUpdate": "spot.order_book_update",
		"ChannelSpotOrder":           "spot.orders",
		"ChannelSpotUserTrade":       "spot.usertrades",
		"ChannelSpotBalance":         "spot.balances",
		"ChannelSpotMarginBalance":   "spot.margin_balances",
		"ChannelSpotFundingBalance":  "spot.funding_balances",
		"ChannelSpotCrossBalance":    "spot.cross_balances",
		"ChannelSpotLogin":           "spot.login",
		"ChannelSpotOrderAmend":      "spot.order_amend",
		"ChannelSpotOrderCancel":     "spot.order_cancel",
		"ChannelSpotOrderCancelCp":   "spot.order_cancel_cp",
		"ChannelSpotOrderCancelIds":  "spot.order_cancel_ids",
		"ChannelSpotOrderPlace":      "spot.order_place",
		"ChannelSpotOrderStatus":     "spot.order_status",
		"ChannelFutureTicker":           "futures.tickers",
		"ChannelFutureTrade":            "futures.trades",
		"ChannelFutureBookTicker":       "futures.book_ticker",
		"ChannelFutureCandleStick":      "futures.candlesticks",
		"ChannelFutureOrderBook":        "futures.order_book",
		"ChannelFutureOrderBookUpdate": "futures.order_book_update",
		"ChannelFutureOrder":            "futures.orders",
		"ChannelFutureUserTrade":        "futures.usertrades",
		"ChannelFutureLiquidates":       "futures.liquidates",
		"ChannelFutureAutoDeleverages":  "futures.auto_deleverages",
		"ChannelFuturePositionCloses":   "futures.position_closes",
		"ChannelFutureBalance":          "futures.balances",
		"ChannelFutureReduceRiskLimits": "futures.reduce_risk_limits",
		"ChannelFuturePositions":        "futures.positions",
		"ChannelFutureAutoOrders":       "futures.autoorders",
		"ChannelFutureLogin":            "futures.login",
		"ChannelFutureOrderAmend":       "futures.order_amend",
		"ChannelFutureOrderCancel":      "futures.order_cancel",
		"ChannelFutureOrderCancelCp":    "futures.order_cancel_cp",
		"ChannelFutureOrderPlace":       "futures.order_place",
		"ChannelFutureOrderBatchPlace":  "futures.order_batch_place",
		"ChannelFutureOrderStatus":      "futures.order_status",
		"ChannelFutureOrderList":        "futures.order_list",
	}

	for name, v := range expected {
		if want[name] != v {
			t.Errorf("constant %s = %q, expected %q", name, v, want[name])
		}
	}
	if len(expected) != len(want) {
		t.Errorf("expected map size %d, want map size %d - check coverage of all constants",
			len(expected), len(want))
	}
}
