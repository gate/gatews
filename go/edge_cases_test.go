package gatews

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// TestSubscribePingChannelNotStoredInHistory verifies the explicit early
// return in baseSubscribe for channels ending in "ping": the wire request
// is sent, but the channel is NOT recorded in subscribeMsg history. This
// matters because reconnect would otherwise replay heartbeat subs and cause
// duplicate ping floods.
func TestSubscribePingChannelNotStoredInHistory(t *testing.T) {
	clientConn, serverConn := newWebSocketPair(t)
	ws, cancel := newTestWsService(t, clientConn, &ConfOptions{
		URL:              "ws://example.com/ws",
		MaxRetryConn:     1,
		ShowReconnectMsg: false,
		PingInterval:     "24h",
	})
	defer cancel()

	go func() {
		// drain server
		_, _, _ = serverConn.ReadMessage()
	}()

	if err := ws.Subscribe("spot.ping", nil); err != nil {
		t.Fatalf("Subscribe ping err: %v", err)
	}

	if _, ok := ws.conf.subscribeMsg.Load("spot.ping"); ok {
		t.Fatal("'spot.ping' must NOT be stored in subscribe history (would cause duplicate replay on reconnect)")
	}
	// But msgCh should still be registered
	if _, ok := ws.msgChs.Load("spot.ping"); !ok {
		t.Fatal("'spot.ping' msgCh should still be registered for incoming pong handling")
	}
}

// TestSubscribeTimeChannelNotStoredInHistory complements the ping test:
// channels ending in ".time" are also intentionally excluded from
// subscribe history (they're typically transient time-sync queries).
func TestSubscribeTimeChannelNotStoredInHistory(t *testing.T) {
	clientConn, serverConn := newWebSocketPair(t)
	ws, cancel := newTestWsService(t, clientConn, &ConfOptions{
		URL:              "ws://example.com/ws",
		MaxRetryConn:     1,
		ShowReconnectMsg: false,
		PingInterval:     "24h",
	})
	defer cancel()

	go func() {
		_, _, _ = serverConn.ReadMessage()
	}()

	if err := ws.Subscribe("spot.time", nil); err != nil {
		t.Fatalf("Subscribe time err: %v", err)
	}

	if _, ok := ws.conf.subscribeMsg.Load("spot.time"); ok {
		t.Fatal("'spot.time' must NOT be stored in subscribe history")
	}
}

// TestSubscribeAccumulatesHistoryAcrossMultipleCalls verifies that
// repeated Subscribe calls for the same channel append history entries
// (used so reconnect replays the latest payload state).
func TestSubscribeAccumulatesHistoryAcrossMultipleCalls(t *testing.T) {
	clientConn, serverConn := newWebSocketPair(t)
	ws, cancel := newTestWsService(t, clientConn, &ConfOptions{
		URL:              "ws://example.com/ws",
		MaxRetryConn:     1,
		ShowReconnectMsg: false,
		PingInterval:     "24h",
	})
	defer cancel()

	go func() {
		for {
			if _, _, err := serverConn.ReadMessage(); err != nil {
				return
			}
		}
	}()

	for _, market := range []string{"BTC_USDT", "ETH_USDT", "DOGE_USDT"} {
		if err := ws.Subscribe(ChannelSpotPublicTrade, []string{market}); err != nil {
			t.Fatalf("Subscribe %s err: %v", market, err)
		}
	}

	v, ok := ws.conf.subscribeMsg.Load(ChannelSpotPublicTrade)
	if !ok {
		t.Fatal("expected subscribe history for spot.trades")
	}
	history := v.([]requestHistory)
	if len(history) != 3 {
		t.Fatalf("expected 3 history entries, got %d", len(history))
	}
}

// TestNewConnConfFromOptionEmptyAppDefaultsToSpot verifies that omitting
// App in ConfOptions results in App="" (not "spot") at the conf level —
// applyOptionConf later fills "spot" only when called via NewWsService.
// This documents the actual behavior so callers know NOT to rely on
// NewConnConfFromOption alone for the App default.
func TestNewConnConfFromOptionEmptyAppLeavesEmpty(t *testing.T) {
	conf := NewConnConfFromOption(&ConfOptions{})
	if conf.App != "" {
		t.Fatalf("NewConnConfFromOption sets App=%q on empty input; "+
			"applyOptionConf is responsible for the spot default. If this "+
			"test fails, callers may have started relying on the default", conf.App)
	}
}

// TestApplyOptionConfFillsAppDefault verifies the spot default is applied
// when going through the full NewWsService path.
func TestApplyOptionConfFillsAppDefault(t *testing.T) {
	clientConn, _ := newWebSocketPair(t)

	originalDial := dialWebSocket
	dialWebSocket = func(*websocket.Dialer, string, http.Header) (*websocket.Conn, *http.Response, error) {
		return clientConn, nil, nil
	}
	t.Cleanup(func() { dialWebSocket = originalDial })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ws, err := NewWsService(ctx, log.New(io.Discard, "", 0), &ConnConf{
		// no App set
		URL:              "ws://example.com/ws",
		MaxRetryConn:     1,
		ShowReconnectMsg: false,
		PingInterval:     "24h",
		subscribeMsg:     new(sync.Map),
	})
	if err != nil {
		t.Fatalf("NewWsService err: %v", err)
	}
	if ws.conf.App != "spot" {
		t.Errorf("App should default to 'spot', got %q", ws.conf.App)
	}
}

// TestActivePingInvalidPingIntervalFallsBackToDefault verifies the resilience
// of activePing parsing: an unparseable PingInterval string should fall
// back to the package default ("10s") rather than panic.
func TestActivePingInvalidPingIntervalFallsBackToDefault(t *testing.T) {
	clientConn, _ := newWebSocketPair(t)

	logBuf := &bytes.Buffer{}
	ws, cancel := newTestWsService(t, clientConn, &ConfOptions{
		URL:              "ws://example.com/ws",
		MaxRetryConn:     1,
		ShowReconnectMsg: false,
		PingInterval:     "not-a-duration",
	})
	defer cancel()
	ws.Logger = log.New(logBuf, "", 0)

	// Run activePing in a short-lived goroutine so we can observe the
	// "failed to parse ping interval" log line emitted during fallback.
	done := make(chan struct{})
	go func() {
		defer close(done)
		ws.activePing()
	}()

	// Give it a moment to log the parse failure
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("activePing did not exit after context cancel")
	}

	if !bytes.Contains(logBuf.Bytes(), []byte("failed to parse ping interval")) {
		t.Errorf("expected fallback log; got %q", logBuf.String())
	}
}

// TestReconnectShowReconnectMsgFalseSuppressesPerChannelLog ensures that
// setting ShowReconnectMsg=false doesn't emit per-channel resubscribe
// log lines (cosmetic feature but part of the contract).
func TestReconnectShowReconnectMsgFalseSuppressesPerChannelLog(t *testing.T) {
	oldClientConn, _ := newWebSocketPair(t)
	newClientConn, newServerConn := newWebSocketPair(t)

	originalDial := dialWebSocket
	dialWebSocket = func(*websocket.Dialer, string, http.Header) (*websocket.Conn, *http.Response, error) {
		return newClientConn, nil, nil
	}
	t.Cleanup(func() { dialWebSocket = originalDial })

	logBuf := &bytes.Buffer{}
	ws, cancel := newTestWsService(t, oldClientConn, &ConfOptions{
		URL:              "ws://example.com/ws",
		MaxRetryConn:     1,
		ShowReconnectMsg: false, // <-- suppress
		PingInterval:     "24h",
	})
	defer cancel()
	ws.Logger = log.New(logBuf, "", 0)

	ws.conf.subscribeMsg.Store(ChannelSpotPublicTrade, []requestHistory{
		{Channel: ChannelSpotPublicTrade, Event: Subscribe, Payload: []string{"BTC_USDT"}},
	})

	go func() {
		// drain reconnect resubscribe
		_, _, _ = newServerConn.ReadMessage()
	}()

	if err := ws.reconnect(); err != nil {
		t.Fatalf("reconnect err: %v", err)
	}

	if bytes.Contains(logBuf.Bytes(), []byte("reconnect channel")) {
		t.Errorf("ShowReconnectMsg=false should suppress per-channel log; got %q", logBuf.String())
	}
}

// TestReconnectShowReconnectMsgTrueEmitsPerChannelLog is the converse:
// when ShowReconnectMsg=true, each replayed channel logs a confirmation.
func TestReconnectShowReconnectMsgTrueEmitsPerChannelLog(t *testing.T) {
	oldClientConn, _ := newWebSocketPair(t)
	newClientConn, newServerConn := newWebSocketPair(t)

	originalDial := dialWebSocket
	dialWebSocket = func(*websocket.Dialer, string, http.Header) (*websocket.Conn, *http.Response, error) {
		return newClientConn, nil, nil
	}
	t.Cleanup(func() { dialWebSocket = originalDial })

	logBuf := &bytes.Buffer{}
	ws, cancel := newTestWsService(t, oldClientConn, &ConfOptions{
		URL:              "ws://example.com/ws",
		MaxRetryConn:     1,
		ShowReconnectMsg: true,
		PingInterval:     "24h",
	})
	defer cancel()
	ws.Logger = log.New(logBuf, "", 0)

	ws.conf.subscribeMsg.Store(ChannelSpotPublicTrade, []requestHistory{
		{Channel: ChannelSpotPublicTrade, Event: Subscribe, Payload: []string{"BTC_USDT"}},
	})

	go func() {
		_, _, _ = newServerConn.ReadMessage()
	}()

	if err := ws.reconnect(); err != nil {
		t.Fatalf("reconnect err: %v", err)
	}

	if !bytes.Contains(logBuf.Bytes(), []byte("reconnect channel")) {
		t.Errorf("ShowReconnectMsg=true should log per-channel; got %q", logBuf.String())
	}
}

// TestReconnectAvoidsRepeatedReconnection verifies the early-return guard:
// if status is already "reconnecting", reconnect() returns nil immediately
// without attempting another dial.
func TestReconnectAvoidsRepeatedReconnection(t *testing.T) {
	clientConn, _ := newWebSocketPair(t)

	dialCalls := 0
	originalDial := dialWebSocket
	dialWebSocket = func(*websocket.Dialer, string, http.Header) (*websocket.Conn, *http.Response, error) {
		dialCalls++
		return clientConn, nil, nil
	}
	t.Cleanup(func() { dialWebSocket = originalDial })

	ws, cancel := newTestWsService(t, clientConn, &ConfOptions{
		URL:              "ws://example.com/ws",
		MaxRetryConn:     1,
		ShowReconnectMsg: false,
		PingInterval:     "24h",
	})
	defer cancel()

	ws.status = reconnecting // pretend a reconnect is already in flight
	if err := ws.reconnect(); err != nil {
		t.Fatalf("reconnect err: %v", err)
	}
	if dialCalls != 0 {
		t.Fatalf("should not dial when already reconnecting; got %d", dialCalls)
	}
}

// TestUpdateMsgFullJSONUnmarshal exercises every field of UpdateMsg in one
// go to catch JSON tag drift on the model.
func TestUpdateMsgFullJSONUnmarshal(t *testing.T) {
	src := `{
		"header": {
			"response_time": "2026-04-29T00:00:00Z",
			"status": "200",
			"channel": "spot.order_place",
			"event": "api",
			"client_id": "cid"
		},
		"time": 1700000000,
		"time_ms": 1700000000123,
		"id": 42,
		"channel": "spot.order_place",
		"event": "api",
		"error": {"code": 4001, "message": "auth"},
		"result": null,
		"data": {
			"result": {"order_id": "abc"},
			"errs": null
		}
	}`

	var msg UpdateMsg
	if err := json.Unmarshal([]byte(src), &msg); err != nil {
		t.Fatalf("unmarshal err: %v", err)
	}
	if msg.Time != 1700000000 {
		t.Errorf("Time mismatch: %d", msg.Time)
	}
	if msg.TimeMs != 1700000000123 {
		t.Errorf("TimeMs mismatch: %d", msg.TimeMs)
	}
	if msg.Id == nil || *msg.Id != 42 {
		t.Errorf("Id mismatch: %v", msg.Id)
	}
	if msg.Error == nil || msg.Error.Code != 4001 {
		t.Errorf("Error mismatch: %+v", msg.Error)
	}
	if msg.Channel != "spot.order_place" {
		t.Errorf("Channel mismatch: %q", msg.Channel)
	}
	if msg.Event != "api" {
		t.Errorf("Event mismatch: %q", msg.Event)
	}
}

// TestRequestSerializationAuthFields ensures the Auth struct serializes
// with the exact field names Gate's API expects: "method", "KEY", "SIGN"
// (uppercase). A casing change would silently break authentication.
func TestRequestSerializationAuthFields(t *testing.T) {
	req := Request{
		Time:    1700000000,
		Channel: "spot.orders",
		Event:   Subscribe,
		Auth: Auth{
			Method: AuthMethodApiKey,
			Key:    "K",
			Secret: "deadbeef",
		},
	}
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal err: %v", err)
	}
	body := string(raw)
	for _, want := range []string{`"method":"api_key"`, `"KEY":"K"`, `"SIGN":"deadbeef"`} {
		if !strings.Contains(body, want) {
			t.Errorf("auth wire format missing %q in %s", want, body)
		}
	}
}

// TestNewWsServiceWithCancelledContextStillSucceeds documents that
// NewWsService does NOT abort on cancelled context (it only uses ctx
// for downstream goroutine lifecycle); dial succeeds independently.
func TestNewWsServiceWithCancelledContextStillSucceeds(t *testing.T) {
	clientConn, _ := newWebSocketPair(t)

	originalDial := dialWebSocket
	dialWebSocket = func(*websocket.Dialer, string, http.Header) (*websocket.Conn, *http.Response, error) {
		return clientConn, nil, nil
	}
	t.Cleanup(func() { dialWebSocket = originalDial })

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled

	ws, err := NewWsService(ctx, log.New(io.Discard, "", 0), &ConnConf{
		URL:              "ws://example.com/ws",
		MaxRetryConn:     1,
		ShowReconnectMsg: false,
		PingInterval:     "24h",
		subscribeMsg:     new(sync.Map),
	})
	if err != nil {
		t.Fatalf("NewWsService should succeed even with cancelled ctx: %v", err)
	}
	if ws == nil {
		t.Fatal("expected non-nil ws")
	}
}

// TestReconnectMaxRetryReachedReturnsError verifies that reconnect()
// gives up after MaxRetryConn dial failures and surfaces the underlying
// error to the caller.
func TestReconnectMaxRetryReachedReturnsError(t *testing.T) {
	clientConn, _ := newWebSocketPair(t)

	originalDial := dialWebSocket
	dialWebSocket = func(*websocket.Dialer, string, http.Header) (*websocket.Conn, *http.Response, error) {
		return nil, nil, errors.New("persistent dial fail")
	}
	t.Cleanup(func() { dialWebSocket = originalDial })

	ws, cancel := newTestWsService(t, clientConn, &ConfOptions{
		URL:              "ws://example.com/ws",
		MaxRetryConn:     1,
		ShowReconnectMsg: false,
		PingInterval:     "24h",
	})
	defer cancel()

	if err := ws.reconnect(); err == nil {
		t.Fatal("expected dial-failure error after exhausting retries")
	}
}
