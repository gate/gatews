package gatews

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"reflect"
	"sync"
	"testing"
	"time"
)

// TestReceiveCallMsgExitsOnContextCancel verifies that the per-channel
// goroutine spawned by receiveCallMsg terminates promptly when the parent
// context is cancelled. Failing this would leak a goroutine per subscribed
// channel on every shutdown.
func TestReceiveCallMsgExitsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	ws := &WsService{
		Ctx:    ctx,
		Logger: log.New(io.Discard, "", 0),
		calls:  new(sync.Map),
	}

	msgCh := make(chan *UpdateMsg, 1)
	exited := make(chan struct{})
	go func() {
		ws.receiveCallMsg(ChannelSpotPublicTrade, msgCh)
		close(exited)
	}()

	// Goroutine must be alive
	select {
	case <-exited:
		t.Fatal("receiveCallMsg exited prematurely")
	case <-time.After(50 * time.Millisecond):
	}

	cancel()

	select {
	case <-exited:
	case <-time.After(2 * time.Second):
		t.Fatal("receiveCallMsg did not exit after context cancel - goroutine leak")
	}
}

// TestReceiveCallMsgInvokesCallbackOnMessage verifies the message-dispatch
// branch of receiveCallMsg: when a message is delivered to msgCh, the
// callback registered in calls is invoked.
func TestReceiveCallMsgInvokesCallbackOnMessage(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ws := &WsService{
		Ctx:    ctx,
		Logger: log.New(io.Discard, "", 0),
		calls:  new(sync.Map),
	}

	got := make(chan *UpdateMsg, 1)
	ws.SetCallBack(ChannelSpotPublicTrade, NewCallBack(func(msg *UpdateMsg) {
		got <- msg
	}))

	msgCh := make(chan *UpdateMsg, 1)
	go ws.receiveCallMsg(ChannelSpotPublicTrade, msgCh)

	want := &UpdateMsg{Channel: ChannelSpotPublicTrade, Event: "update"}
	msgCh <- want

	select {
	case received := <-got:
		if received != want {
			t.Fatalf("unexpected msg dispatched: got %+v want %+v", received, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("callback was not invoked")
	}
}

// TestSetCallBackOverridesPreviousCallback verifies that calling SetCallBack
// twice on the same channel replaces the earlier callback (last-write-wins).
func TestSetCallBackOverridesPreviousCallback(t *testing.T) {
	ws := &WsService{calls: new(sync.Map)}

	first := 0
	second := 0
	ws.SetCallBack(ChannelSpotPublicTrade, NewCallBack(func(*UpdateMsg) { first++ }))
	ws.SetCallBack(ChannelSpotPublicTrade, NewCallBack(func(*UpdateMsg) { second++ }))

	if loaded, ok := ws.calls.Load(ChannelSpotPublicTrade); ok {
		loaded.(CallBack)(&UpdateMsg{})
	} else {
		t.Fatal("callback not registered")
	}

	if first != 0 {
		t.Errorf("first callback was invoked %d times after override", first)
	}
	if second != 1 {
		t.Errorf("second callback expected 1 invocation, got %d", second)
	}
}

// TestSetCallBackNilIsNoop verifies that passing nil to SetCallBack does
// not register an entry (which would NPE later when invoked).
func TestSetCallBackNilIsNoop(t *testing.T) {
	ws := &WsService{calls: new(sync.Map)}
	ws.SetCallBack(ChannelSpotPublicTrade, nil)

	if _, ok := ws.calls.Load(ChannelSpotPublicTrade); ok {
		t.Fatal("nil callback was registered; expected no-op")
	}
}

// TestConcurrentSubscribesToDifferentChannelsRegisterAll exercises the
// thread-safety of msgChs / subscribeMsg sync.Map under concurrent
// Subscribe calls to disjoint channels.
func TestConcurrentSubscribesToDifferentChannelsRegisterAll(t *testing.T) {
	clientConn, serverConn := newWebSocketPair(t)
	ws, cancel := newTestWsService(t, clientConn, &ConfOptions{
		URL:              "ws://example.com/ws",
		MaxRetryConn:     1,
		ShowReconnectMsg: false,
		PingInterval:     "24h",
	})
	defer cancel()

	// Drain server to avoid blocking client writes
	go func() {
		for {
			if _, _, err := serverConn.ReadMessage(); err != nil {
				return
			}
		}
	}()

	channels := []string{
		ChannelSpotPublicTrade,
		ChannelSpotTicker,
		ChannelSpotBookTicker,
		ChannelSpotCandleStick,
		ChannelSpotOrderBookUpdate,
	}

	var wg sync.WaitGroup
	wg.Add(len(channels))
	errors := make(chan error, len(channels))

	for _, ch := range channels {
		ch := ch
		go func() {
			defer wg.Done()
			if err := ws.Subscribe(ch, []string{"BTC_USDT"}); err != nil {
				errors <- err
			}
		}()
	}
	wg.Wait()
	close(errors)

	for err := range errors {
		t.Errorf("concurrent Subscribe err: %v", err)
	}

	// Each channel should have its own msgCh registered
	for _, ch := range channels {
		if _, ok := ws.msgChs.Load(ch); !ok {
			t.Errorf("channel %q not registered after concurrent Subscribe", ch)
		}
		if _, ok := ws.conf.subscribeMsg.Load(ch); !ok {
			t.Errorf("channel %q not in subscribeMsg history", ch)
		}
	}
}

// TestRequestHistoryJSONRoundTrip verifies that the requestHistory struct
// preserves channel/event when round-tripped through JSON. (Reconnect
// resubscribe correctness is covered end-to-end by
// TestReconnectResubscribesChannelsE2E in channel_test.go.)
func TestRequestHistoryJSONRoundTrip(t *testing.T) {
	rh := requestHistory{
		Channel: ChannelSpotOrderBookUpdate,
		Event:   Subscribe,
		Payload: []string{"BTC_USDT", "ETH_USDT", "100ms"},
	}
	raw, err := json.Marshal(rh)
	if err != nil {
		t.Fatalf("marshal err: %v", err)
	}
	var got requestHistory
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal err: %v", err)
	}
	if got.Channel != rh.Channel {
		t.Errorf("channel lost: got %q want %q", got.Channel, rh.Channel)
	}
	if got.Event != rh.Event {
		t.Errorf("event lost: got %q want %q", got.Event, rh.Event)
	}
	// Payload is `any`; after JSON it deserializes as []any{...}
	gotPayload, ok := got.Payload.([]any)
	if !ok {
		t.Fatalf("payload type lost: %T", got.Payload)
	}
	if len(gotPayload) != 3 {
		t.Fatalf("payload length lost: got %d", len(gotPayload))
	}
	if gotPayload[0] != "BTC_USDT" || gotPayload[2] != "100ms" {
		t.Errorf("payload content corrupted: %v", gotPayload)
	}
}

// TestAuthChannelMapMatchesConstants asserts that every key in authChannel
// is one of the canonical channel constants. This is a regression guard:
// if someone adds a private channel constant but forgets to register it
// in authChannel, baseSubscribe would not enforce credentials and the
// server would reject the request silently.
func TestAuthChannelMapMatchesConstants(t *testing.T) {
	expected := map[string]bool{
		ChannelSpotBalance:            true,
		ChannelSpotFundingBalance:     true,
		ChannelSpotMarginBalance:      true,
		ChannelSpotOrder:              true,
		ChannelSpotUserTrade:          true,
		ChannelFutureOrder:            true,
		ChannelFutureUserTrade:        true,
		ChannelFutureLiquidates:       true,
		ChannelFutureAutoDeleverages:  true,
		ChannelFuturePositionCloses:   true,
		ChannelFutureReduceRiskLimits: true,
		ChannelFuturePositions:        true,
		ChannelFutureAutoOrders:       true,
		ChannelFutureBalance:          true,
	}
	if !reflect.DeepEqual(authChannel, expected) {
		t.Errorf("authChannel diverges from expected. authChannel=%v, expected=%v",
			authChannel, expected)
	}
}

// TestStatusStringForKnownStates verifies the human-readable status mapping.
func TestStatusStringForKnownStates(t *testing.T) {
	for _, tc := range []struct {
		s    status
		want string
	}{
		{disconnected, "disconnected"},
		{connected, "connected"},
		{reconnecting, "reconnecting"},
	} {
		ws := &WsService{status: tc.s}
		if got := ws.Status(); got != tc.want {
			t.Errorf("status(%d): got %q want %q", tc.s, got, tc.want)
		}
	}
}

// TestStatusStringForUnknownStateIsEmpty exercises the defensive branch
// where status is set to a value outside the defined enum (defensive code
// for forward-compatibility).
func TestStatusStringForUnknownStateIsEmpty(t *testing.T) {
	ws := &WsService{status: status(999)}
	if got := ws.Status(); got != "" {
		t.Errorf("unknown status should map to empty string, got %q", got)
	}
}

// TestSubscribeOptionsIDPropagatesToRequest ensures that an explicit ID
// on SubscribeOptions ends up in the wire request. Complements
// TestSubscribeFuturesWithOptionsE2E by isolating the ID-propagation path.
func TestSubscribeOptionsIDPropagatesToRequest(t *testing.T) {
	clientConn, serverConn := newWebSocketPair(t)
	ws, cancel := newTestWsService(t, clientConn, &ConfOptions{
		URL:              "ws://example.com/ws",
		MaxRetryConn:     1,
		ShowReconnectMsg: false,
		PingInterval:     "24h",
	})
	defer cancel()

	got := make(chan int64, 1)
	go func() {
		_, raw, err := serverConn.ReadMessage()
		if err != nil {
			t.Errorf("read err: %v", err)
			return
		}
		var req Request
		if err := json.Unmarshal(raw, &req); err != nil {
			t.Errorf("unmarshal err: %v", err)
			return
		}
		if req.Id == nil {
			got <- 0
			return
		}
		got <- *req.Id
	}()

	if err := ws.SubscribeWithOption(
		ChannelSpotPublicTrade,
		[]string{"BTC_USDT"},
		&SubscribeOptions{ID: 999888},
	); err != nil {
		t.Fatalf("SubscribeWithOption err: %v", err)
	}

	select {
	case id := <-got:
		if id != 999888 {
			t.Errorf("unexpected id: got %d want 999888", id)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out reading subscribe request")
	}
}
