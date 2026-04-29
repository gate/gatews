//go:build integration
// +build integration

// Live integration tests against the actual Gate WebSocket servers.
//
// These tests are excluded from the default test run because they require
// network access and depend on real market activity.
//
// Run with:
//   GOWORK=off go test -tags=integration -count=1 -v -run TestLive ./...
//
// Optional environment overrides:
//   GATE_LIVE_TIMEOUT=30s     how long to wait for data on each channel
//   GATE_LIVE_SYMBOL=BTC_USDT trading pair to subscribe to
//   GATE_LIVE_FUTURES_URL=... override futures endpoint (default usdt-margined)
package gatews

import (
	"context"
	"fmt"
	"log"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// liveChannelCase describes one public channel + its subscribe payload.
type liveChannelCase struct {
	constant    string
	payload     []string
	isKey       bool   // when true: failure to receive any data is a test failure
	description string // human-readable summary for the t.Log output
}

func defaultSymbol() string {
	if v := os.Getenv("GATE_LIVE_SYMBOL"); v != "" {
		return v
	}
	return "BTC_USDT"
}

func liveTimeout(t *testing.T) time.Duration {
	if v := os.Getenv("GATE_LIVE_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
		t.Logf("invalid GATE_LIVE_TIMEOUT=%q, using default", v)
	}
	return 30 * time.Second
}

func liveSpotCases() []liveChannelCase {
	sym := defaultSymbol()
	return []liveChannelCase{
		{ChannelSpotPublicTrade, []string{sym}, true, "public trade ticks"},
		{ChannelSpotTicker, []string{sym}, true, "ticker (price summary)"},
		{ChannelSpotBookTicker, []string{sym}, true, "best bid/ask"},
		{ChannelSpotOrderBookUpdate, []string{sym, "100ms"}, true, "order-book diff updates"},
		// Lower-frequency / snapshot-style channels: subscribe should still
		// succeed and emit the initial state, but data is not strictly
		// required within the test window.
		{ChannelSpotCandleStick, []string{"1m", sym}, false, "1m candlestick"},
		{ChannelSpotOrderBook, []string{sym, "5", "1000ms"}, false, "depth-5 order-book snapshot"},
	}
}

func liveFuturesCases() []liveChannelCase {
	sym := defaultSymbol()
	return []liveChannelCase{
		{ChannelFutureTicker, []string{sym}, true, "futures ticker"},
		{ChannelFutureTrade, []string{sym}, true, "futures trade ticks"},
		{ChannelFutureBookTicker, []string{sym}, true, "futures best bid/ask"},
		{ChannelFutureOrderBookUpdate, []string{sym, "100ms"}, true, "futures order-book diff"},
		{ChannelFutureCandleStick, []string{"1m", sym}, false, "1m futures candlestick"},
		{ChannelFutureOrderBook, []string{sym, "5", "1000ms"}, false, "depth-5 futures order-book"},
	}
}

// liveCounter tracks the number of incoming messages and the timestamp of
// the first data ("update") event for one channel.
type liveCounter struct {
	total       int64 // any message including subscribe ack
	dataUpdates int64 // event == "update" only
	firstSeen   atomic.Int64
}

func (c *liveCounter) record(event string) {
	atomic.AddInt64(&c.total, 1)
	if event == "update" {
		atomic.AddInt64(&c.dataUpdates, 1)
		// Compare-and-swap to record only the first data timestamp
		if c.firstSeen.Load() == 0 {
			c.firstSeen.CompareAndSwap(0, time.Now().UnixMilli())
		}
	}
}

// runLiveSubscriptionTable subscribes to every case in `cases` against the
// already-connected `ws`, waits up to `timeout` for data on key channels,
// and reports per-channel counts in the test log. Failures are reported
// only for key channels that received zero data updates.
func runLiveSubscriptionTable(t *testing.T, ws *WsService, cases []liveChannelCase, timeout time.Duration) {
	t.Helper()

	counters := make(map[string]*liveCounter, len(cases))
	for _, c := range cases {
		counters[c.constant] = &liveCounter{}
	}

	// Register callbacks BEFORE subscribing so we don't miss the first updates
	for _, c := range cases {
		ch := c.constant
		ws.SetCallBack(ch, NewCallBack(func(msg *UpdateMsg) {
			counters[ch].record(msg.Event)
		}))
	}

	for _, c := range cases {
		if err := ws.Subscribe(c.constant, c.payload); err != nil {
			t.Errorf("Subscribe %q with payload %v failed: %v", c.constant, c.payload, err)
		}
	}

	// Wait until either every key channel has at least one data update, or
	// timeout elapses.
	deadline := time.Now().Add(timeout)
	allHaveData := func() bool {
		for _, c := range cases {
			if c.isKey && atomic.LoadInt64(&counters[c.constant].dataUpdates) == 0 {
				return false
			}
		}
		return true
	}
	for time.Now().Before(deadline) && !allHaveData() {
		time.Sleep(250 * time.Millisecond)
	}

	// Report
	for _, c := range cases {
		ctr := counters[c.constant]
		total := atomic.LoadInt64(&ctr.total)
		updates := atomic.LoadInt64(&ctr.dataUpdates)
		first := ctr.firstSeen.Load()
		latency := ""
		if first != 0 {
			latency = fmt.Sprintf(", first update at +%dms", time.Now().UnixMilli()-first)
		}
		marker := "·"
		if c.isKey {
			marker = "★"
		}

		switch {
		case c.isKey && updates == 0:
			t.Errorf("%s KEY channel %q (%s): NO DATA in %s (total msgs=%d, none with event=update)",
				marker, c.constant, c.description, timeout, total)
		case updates == 0:
			t.Logf("%s [non-key] %q (%s): no data updates in %s — OK if low-frequency. total msgs=%d",
				marker, c.constant, c.description, timeout, total)
		default:
			t.Logf("%s %q (%s): %d data updates%s (total msgs=%d)",
				marker, c.constant, c.description, updates, latency, total)
		}
	}
}

// TestLiveSpotPublicChannelsReceiveData connects to Gate's spot WebSocket
// and exercises every public channel. Each key channel must receive at
// least one event=update message within the timeout.
func TestLiveSpotPublicChannelsReceiveData(t *testing.T) {
	ws, err := NewWsService(
		context.Background(),
		log.New(os.Stderr, "[gate-spot] ", log.LstdFlags|log.Lmicroseconds),
		NewConnConfFromOption(&ConfOptions{
			URL:              BaseUrl,
			MaxRetryConn:     2,
			ShowReconnectMsg: true,
			PingInterval:     "10s",
		}),
	)
	if err != nil {
		t.Skipf("could not connect to Gate spot WS at %s: %v", BaseUrl, err)
	}
	defer func() {
		if conn := ws.GetConnection(); conn != nil {
			_ = conn.Close()
		}
	}()

	runLiveSubscriptionTable(t, ws, liveSpotCases(), liveTimeout(t))
}

// TestLiveFuturesPublicChannelsReceiveData is the futures counterpart.
func TestLiveFuturesPublicChannelsReceiveData(t *testing.T) {
	url := os.Getenv("GATE_LIVE_FUTURES_URL")
	if url == "" {
		url = FuturesUsdtUrl
	}
	ws, err := NewWsService(
		context.Background(),
		log.New(os.Stderr, "[gate-fut] ", log.LstdFlags|log.Lmicroseconds),
		NewConnConfFromOption(&ConfOptions{
			App:              "futures",
			URL:              url,
			MaxRetryConn:     2,
			ShowReconnectMsg: true,
			PingInterval:     "10s",
		}),
	)
	if err != nil {
		t.Skipf("could not connect to Gate futures WS at %s: %v", url, err)
	}
	defer func() {
		if conn := ws.GetConnection(); conn != nil {
			_ = conn.Close()
		}
	}()

	runLiveSubscriptionTable(t, ws, liveFuturesCases(), liveTimeout(t))
}

// TestLiveSpotSubscribeAcksAllPublicChannels validates a weaker invariant:
// every channel ACCEPTS the subscribe (no error response), even if real
// market data hasn't yet flowed. Useful when the test runs during a market
// halt or when the chosen symbol is illiquid.
func TestLiveSpotSubscribeAcksAllPublicChannels(t *testing.T) {
	ws, err := NewWsService(
		context.Background(),
		log.New(os.Stderr, "[gate-spot-ack] ", log.LstdFlags),
		NewConnConfFromOption(&ConfOptions{
			URL:              BaseUrl,
			MaxRetryConn:     2,
			ShowReconnectMsg: false,
			PingInterval:     "10s",
		}),
	)
	if err != nil {
		t.Skipf("could not connect to spot Gate WS: %v", err)
	}
	defer func() {
		if conn := ws.GetConnection(); conn != nil {
			_ = conn.Close()
		}
	}()

	type ack struct {
		event   string // expected: "subscribe" or "update"
		message string // any error message reported by server
	}
	results := make(map[string]*ack)
	var mu sync.Mutex

	for _, c := range liveSpotCases() {
		ch := c.constant
		results[ch] = &ack{}
		ws.SetCallBack(ch, NewCallBack(func(msg *UpdateMsg) {
			mu.Lock()
			defer mu.Unlock()
			if results[ch].event == "" {
				results[ch].event = msg.Event
			}
			if msg.Error != nil {
				results[ch].message = msg.Error.Message
			}
		}))
	}

	for _, c := range liveSpotCases() {
		if err := ws.Subscribe(c.constant, c.payload); err != nil {
			t.Errorf("Subscribe %q failed at client side: %v", c.constant, err)
		}
	}

	// Wait briefly for ack
	time.Sleep(5 * time.Second)

	for _, c := range liveSpotCases() {
		mu.Lock()
		got := *results[c.constant]
		mu.Unlock()
		if got.event == "" {
			t.Errorf("channel %q: no response from server (no subscribe ack, no data)", c.constant)
		} else if got.message != "" {
			t.Errorf("channel %q: server reported error: %s", c.constant, got.message)
		} else {
			t.Logf("channel %q: server first event=%q (subscribed OK)", c.constant, got.event)
		}
	}
}
