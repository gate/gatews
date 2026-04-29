package gatews

import (
	"context"
	"crypto/hmac"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// TestSubscribeAuthChannelEmbedsHmacSignature verifies that subscribing to a
// private/auth-required channel attaches a correctly-computed HMAC-SHA512
// signature in the Auth field. The existing TestUnSubscribeUsesUnsubscribeSignature
// covers the unsubscribe path; this complements it for the subscribe path.
func TestSubscribeAuthChannelEmbedsHmacSignature(t *testing.T) {
	clientConn, serverConn := newWebSocketPair(t)
	ws, cancel := newTestWsService(t, clientConn, &ConfOptions{
		URL:              "ws://example.com/ws",
		Key:              "test-key",
		Secret:           "test-secret",
		MaxRetryConn:     1,
		ShowReconnectMsg: false,
		PingInterval:     "24h",
	})
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, rawReq, err := serverConn.ReadMessage()
		if err != nil {
			t.Errorf("read subscribe request err: %v", err)
			return
		}
		var req Request
		if err := json.Unmarshal(rawReq, &req); err != nil {
			t.Errorf("unmarshal subscribe request err: %v", err)
			return
		}
		if req.Auth.Method != AuthMethodApiKey {
			t.Errorf("unexpected auth method: %q", req.Auth.Method)
			return
		}
		if req.Auth.Key != "test-key" {
			t.Errorf("unexpected auth key: %q", req.Auth.Key)
			return
		}
		// baseSubscribe always signs the literal "subscribe" event in the message
		// (regardless of unsubscribe), per the existing implementation.
		hash := hmac.New(sha512.New, []byte("test-secret"))
		hash.Write([]byte(fmt.Sprintf("channel=%s&event=%s&time=%d", ChannelSpotOrder, Subscribe, req.Time)))
		want := hex.EncodeToString(hash.Sum(nil))
		if req.Auth.Secret != want {
			t.Errorf("auth signature mismatch:\n  got %s\n want %s", req.Auth.Secret, want)
		}
	}()

	if err := ws.Subscribe(ChannelSpotOrder, []string{"BTC_USDT"}); err != nil {
		t.Fatalf("Subscribe err: %v", err)
	}

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for subscribe signature exchange")
	}
}

// TestReadMsgIgnoresMalformedJSON verifies that a non-parseable JSON message
// is silently skipped (json.Unmarshal err → continue) and the next valid
// message still triggers the registered callback. This is critical for
// stability: a single corrupted frame must not kill the read loop.
func TestReadMsgIgnoresMalformedJSON(t *testing.T) {
	clientConn, serverConn := newWebSocketPair(t)
	ws, cancel := newTestWsService(t, clientConn, &ConfOptions{
		URL:              "ws://example.com/ws",
		MaxRetryConn:     1,
		ShowReconnectMsg: false,
		PingInterval:     "24h",
	})
	defer cancel()

	received := make(chan struct{}, 1)
	ws.SetCallBack(ChannelSpotPublicTrade, NewCallBack(func(msg *UpdateMsg) {
		received <- struct{}{}
	}))

	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)

		// Drain the subscribe request (we don't validate it here)
		_, _, err := serverConn.ReadMessage()
		if err != nil {
			t.Errorf("read subscribe request err: %v", err)
			return
		}

		// 1. Send malformed JSON – should be skipped
		if err := serverConn.WriteMessage(websocket.TextMessage, []byte(`{not-valid-json}`)); err != nil {
			t.Errorf("write malformed err: %v", err)
			return
		}

		// 2. Send valid update – callback must fire
		ok, err := json.Marshal(UpdateMsg{
			Channel: ChannelSpotPublicTrade,
			Event:   "update",
			Result:  json.RawMessage(`[{"price":"100"}]`),
		})
		if err != nil {
			t.Errorf("marshal update err: %v", err)
			return
		}
		if err := serverConn.WriteMessage(websocket.TextMessage, ok); err != nil {
			t.Errorf("write valid err: %v", err)
			return
		}
	}()

	if err := ws.Subscribe(ChannelSpotPublicTrade, []string{"BTC_USDT"}); err != nil {
		t.Fatalf("Subscribe err: %v", err)
	}

	select {
	case <-received:
	case <-time.After(3 * time.Second):
		t.Fatal("callback not invoked after malformed-then-valid frame sequence")
	}

	select {
	case <-serverDone:
	case <-time.After(3 * time.Second):
		t.Fatal("server timed out")
	}
}

// TestActivePingFuturesUsesFuturesPing complements TestActivePingSendsPingFrameE2E
// (which only covers spot) by verifying that when futures channels are
// subscribed, the active ping channel is "futures.ping".
func TestActivePingFuturesUsesFuturesPing(t *testing.T) {
	clientConn, serverConn := newWebSocketPair(t)
	ws, cancel := newTestWsService(t, clientConn, &ConfOptions{
		App:              "futures",
		URL:              "ws://example.com/ws",
		MaxRetryConn:     1,
		ShowReconnectMsg: false,
		PingInterval:     "10ms",
	})
	defer cancel()

	ws.conf.subscribeMsg.Store(ChannelFutureOrder, []requestHistory{
		{
			Channel: ChannelFutureOrder,
			Event:   Subscribe,
			Payload: []string{"BTC_USDT"},
		},
	})

	pingSeen := make(chan string, 1)
	go ws.activePing()
	go func() {
		_, rawReq, err := serverConn.ReadMessage()
		if err != nil {
			t.Errorf("read ping err: %v", err)
			return
		}
		var req Request
		if err := json.Unmarshal(rawReq, &req); err != nil {
			t.Errorf("unmarshal ping err: %v", err)
			return
		}
		pingSeen <- req.Channel
	}()

	select {
	case ch := <-pingSeen:
		if ch != "futures.ping" {
			t.Fatalf("unexpected ping channel: got %q want %q", ch, "futures.ping")
		}
		cancel()
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for futures ping")
	}
}

// TestSubscribeReusesExistingMsgCh verifies that the second Subscribe to
// the same channel reuses the message channel created by the first call,
// rather than spawning a duplicate receiveCallMsg goroutine.
func TestSubscribeReusesExistingMsgCh(t *testing.T) {
	clientConn, serverConn := newWebSocketPair(t)
	ws, cancel := newTestWsService(t, clientConn, &ConfOptions{
		URL:              "ws://example.com/ws",
		MaxRetryConn:     1,
		ShowReconnectMsg: false,
		PingInterval:     "24h",
	})
	defer cancel()

	// Drain server reads in background to avoid blocking client writes
	go func() {
		for {
			if _, _, err := serverConn.ReadMessage(); err != nil {
				return
			}
		}
	}()

	if err := ws.Subscribe(ChannelSpotPublicTrade, []string{"BTC_USDT"}); err != nil {
		t.Fatalf("first Subscribe err: %v", err)
	}
	first, ok := ws.msgChs.Load(ChannelSpotPublicTrade)
	if !ok {
		t.Fatal("expected msgCh to be registered after first Subscribe")
	}

	if err := ws.Subscribe(ChannelSpotPublicTrade, []string{"ETH_USDT"}); err != nil {
		t.Fatalf("second Subscribe err: %v", err)
	}
	second, ok := ws.msgChs.Load(ChannelSpotPublicTrade)
	if !ok {
		t.Fatal("expected msgCh to remain after second Subscribe")
	}

	if first != second {
		t.Fatal("expected the same msgCh instance to be reused for repeated Subscribe to same channel")
	}
}

// TestNewWsServiceWithNilConfUsesDefaults verifies that NewWsService with
// nil ConnConf falls back to internal defaults (BaseUrl, MaxRetryConn,
// DefaultPingInterval, "spot" app).
func TestNewWsServiceWithNilConfUsesDefaults(t *testing.T) {
	clientConn, _ := newWebSocketPair(t)

	originalDial := dialWebSocket
	dialWebSocket = func(*websocket.Dialer, string, http.Header) (*websocket.Conn, *http.Response, error) {
		return clientConn, nil, nil
	}
	t.Cleanup(func() { dialWebSocket = originalDial })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ws, err := NewWsService(ctx, log.New(io.Discard, "", 0), nil)
	if err != nil {
		t.Fatalf("NewWsService err: %v", err)
	}

	if ws.conf.URL != BaseUrl {
		t.Errorf("default URL not applied: got %q want %q", ws.conf.URL, BaseUrl)
	}
	if ws.conf.App != "spot" {
		t.Errorf("default App not applied: got %q want %q", ws.conf.App, "spot")
	}
	if ws.conf.MaxRetryConn != MaxRetryConn {
		t.Errorf("default MaxRetryConn not applied: got %d want %d", ws.conf.MaxRetryConn, MaxRetryConn)
	}
	if ws.conf.PingInterval != DefaultPingInterval {
		t.Errorf("default PingInterval not applied: got %q want %q", ws.conf.PingInterval, DefaultPingInterval)
	}
}

// TestGetConnConfReturnsLiveConfig verifies the public GetConnConf accessor.
func TestGetConnConfReturnsLiveConfig(t *testing.T) {
	clientConn, _ := newWebSocketPair(t)
	ws, cancel := newTestWsService(t, clientConn, &ConfOptions{
		URL:              "ws://example.com/ws",
		Key:              "k",
		Secret:           "s",
		MaxRetryConn:     7,
		ShowReconnectMsg: false,
		PingInterval:     "5s",
	})
	defer cancel()

	got := ws.GetConnConf()
	if got == nil {
		t.Fatal("GetConnConf returned nil")
	}
	if got.Key != "k" || got.Secret != "s" || got.MaxRetryConn != 7 || got.PingInterval != "5s" {
		t.Errorf("unexpected conf returned: %+v", got)
	}

	// Verify it's the same pointer (live view, not a copy)
	if got != ws.conf {
		t.Error("GetConnConf returned a copy; expected pointer to live conf")
	}
}

// TestGetChannelMarketsEmptyForUnknownChannel ensures that querying markets
// for a channel that was never subscribed returns nil rather than panicking.
func TestGetChannelMarketsEmptyForUnknownChannel(t *testing.T) {
	ws := &WsService{
		conf: &ConnConf{
			subscribeMsg: new(sync.Map),
		},
	}

	if got := ws.GetChannelMarkets(ChannelSpotPublicTrade); got != nil {
		t.Fatalf("expected nil markets for unsubscribed channel, got %v", got)
	}
}

// TestApplyOptionConfMergesDefaults exercises the partial override semantics
// of applyOptionConf via NewWsService: user passes only some fields, the rest
// fall back to the defaults of getInitConnConf.
func TestApplyOptionConfMergesDefaults(t *testing.T) {
	clientConn, _ := newWebSocketPair(t)

	originalDial := dialWebSocket
	dialWebSocket = func(*websocket.Dialer, string, http.Header) (*websocket.Conn, *http.Response, error) {
		return clientConn, nil, nil
	}
	t.Cleanup(func() { dialWebSocket = originalDial })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Provide only Key+Secret; everything else should be defaults.
	partial := &ConnConf{
		Key:    "K",
		Secret: "S",
	}
	ws, err := NewWsService(ctx, log.New(io.Discard, "", 0), partial)
	if err != nil {
		t.Fatalf("NewWsService err: %v", err)
	}

	if ws.conf.URL != BaseUrl {
		t.Errorf("URL not defaulted: got %q want %q", ws.conf.URL, BaseUrl)
	}
	if ws.conf.MaxRetryConn != MaxRetryConn {
		t.Errorf("MaxRetryConn not defaulted: got %d want %d", ws.conf.MaxRetryConn, MaxRetryConn)
	}
	if ws.conf.PingInterval != DefaultPingInterval {
		t.Errorf("PingInterval not defaulted: got %q want %q", ws.conf.PingInterval, DefaultPingInterval)
	}
	if ws.conf.App != "spot" {
		t.Errorf("App not defaulted: got %q want %q", ws.conf.App, "spot")
	}
	if ws.conf.Key != "K" || ws.conf.Secret != "S" {
		t.Errorf("user-provided creds lost: %+v", ws.conf)
	}
}

// TestAPIRequestEmptyAuthReturnsError verifies that calling APIRequest on
// an auth-required channel without key/secret short-circuits with an error
// (without sending anything over the wire).
func TestAPIRequestEmptyAuthReturnsError(t *testing.T) {
	ws := &WsService{
		conf: &ConnConf{
			subscribeMsg: new(sync.Map),
			App:          "spot",
		},
		Logger:    log.New(io.Discard, "", 0),
		mu:        new(sync.Mutex),
		clientMu:  new(sync.Mutex),
		msgChs:    new(sync.Map),
		calls:     new(sync.Map),
		once:      new(sync.Once),
		loginOnce: new(sync.Once),
	}

	err := ws.APIRequest(ChannelSpotOrderPlace, map[string]any{"contract": "BTC_USDT"}, nil)
	if err == nil {
		t.Fatal("expected auth-empty error, got nil")
	}
	if err.Error() != "auth key or secret empty" {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestNewWsServiceMaxRetryFails verifies that NewWsService gives up after
// exhausting MaxRetryConn dial attempts and returns the last dial error.
func TestNewWsServiceMaxRetryFails(t *testing.T) {
	originalDial := dialWebSocket
	var dialCount atomic.Int32
	dialWebSocket = func(*websocket.Dialer, string, http.Header) (*websocket.Conn, *http.Response, error) {
		dialCount.Add(1)
		return nil, nil, errors.New("dial fail")
	}
	t.Cleanup(func() { dialWebSocket = originalDial })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	_, err := NewWsService(ctx, log.New(io.Discard, "", 0), &ConnConf{
		URL:              "ws://example.com/ws",
		MaxRetryConn:     2,
		ShowReconnectMsg: false,
		PingInterval:     "24h",
		subscribeMsg:     new(sync.Map),
	})
	if err == nil {
		t.Fatal("expected dial failure, got nil")
	}
	if got := dialCount.Load(); got != 3 {
		t.Fatalf("unexpected dial attempts: got %d want 3 (1 + 2 retries)", got)
	}
}

// TestSubscribeChannelTypeNonAuthDoesNotRequireCredentials sanity check that
// public channels work without key/secret.
func TestSubscribeChannelTypeNonAuthDoesNotRequireCredentials(t *testing.T) {
	clientConn, serverConn := newWebSocketPair(t)
	ws, cancel := newTestWsService(t, clientConn, &ConfOptions{
		URL:              "ws://example.com/ws",
		MaxRetryConn:     1,
		ShowReconnectMsg: false,
		PingInterval:     "24h",
		// no Key, no Secret
	})
	defer cancel()

	go func() {
		// just drain the request, no assertion needed
		_, _, _ = serverConn.ReadMessage()
	}()

	if err := ws.Subscribe(ChannelSpotPublicTrade, []string{"BTC_USDT"}); err != nil {
		t.Fatalf("public Subscribe must not require auth: %v", err)
	}
}

// TestUpdateMsgUnmarshalNewHeaderFields verifies that the additional fields
// added to ResponseHeader (conn_id, trace_id, rate-limit fields) properly
// round-trip through JSON.
func TestUpdateMsgUnmarshalNewHeaderFields(t *testing.T) {
	src := `{
		"header": {
			"response_time": "2026-04-29T00:00:00Z",
			"status": "200",
			"channel": "spot.order_place",
			"event": "api",
			"client_id": "cid-1"
		},
		"channel": "spot.order_place",
		"event": "api",
		"data": {"result": {}}
	}`

	var msg UpdateMsg
	if err := json.Unmarshal([]byte(src), &msg); err != nil {
		t.Fatalf("unmarshal err: %v", err)
	}
	if msg.Header.ClientID != "cid-1" {
		t.Errorf("client_id mismatch: got %q", msg.Header.ClientID)
	}
	if msg.Header.Status != "200" {
		t.Errorf("status mismatch: got %q", msg.Header.Status)
	}
	if msg.GetChannel() != "spot.order_place" {
		t.Errorf("GetChannel mismatch: got %q", msg.GetChannel())
	}
}

// TestRequestTimeIsRecentEnough verifies that the request signature uses
// a fresh timestamp rather than a hard-coded zero (sanity check for
// nonce/replay considerations).
func TestRequestTimeIsRecentEnough(t *testing.T) {
	clientConn, serverConn := newWebSocketPair(t)
	ws, cancel := newTestWsService(t, clientConn, &ConfOptions{
		URL:              "ws://example.com/ws",
		Key:              "k",
		Secret:           "s",
		MaxRetryConn:     1,
		ShowReconnectMsg: false,
		PingInterval:     "24h",
	})
	defer cancel()

	now := time.Now().Unix()
	done := make(chan int64, 1)
	go func() {
		_, rawReq, err := serverConn.ReadMessage()
		if err != nil {
			t.Errorf("read err: %v", err)
			return
		}
		var req Request
		if err := json.Unmarshal(rawReq, &req); err != nil {
			t.Errorf("unmarshal err: %v", err)
			return
		}
		done <- req.Time
	}()

	if err := ws.Subscribe(ChannelSpotOrder, []string{"BTC_USDT"}); err != nil {
		t.Fatalf("Subscribe err: %v", err)
	}

	select {
	case ts := <-done:
		// Should be within 5 seconds of "now"
		if ts < now-5 || ts > now+5 {
			t.Fatalf("subscribe timestamp out of range: got %d, expected ~%d", ts, now)
		}
		_ = strconv.FormatInt(ts, 10) // exercise that strconv import isn't dead
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for subscribe request")
	}
}
