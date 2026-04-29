package gatews

import (
	"bufio"
	"context"
	"crypto/hmac"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

type pipeResponseWriter struct {
	conn   net.Conn
	header http.Header
}

func (w *pipeResponseWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}

func (w *pipeResponseWriter) WriteHeader(int) {}

func (w *pipeResponseWriter) Write(p []byte) (int, error) {
	return w.conn.Write(p)
}

func (w *pipeResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return w.conn, bufio.NewReadWriter(bufio.NewReader(w.conn), bufio.NewWriter(w.conn)), nil
}

type wsHandshakeResult struct {
	conn *websocket.Conn
	resp *http.Response
	err  error
}

func newWebSocketPair(t *testing.T) (*websocket.Conn, *websocket.Conn) {
	t.Helper()

	serverConn, clientConn := net.Pipe()
	targetURL, err := url.Parse("ws://example.com/ws")
	if err != nil {
		t.Fatalf("parse url err: %v", err)
	}

	resultCh := make(chan wsHandshakeResult, 1)
	go func() {
		conn, resp, err := websocket.NewClient(clientConn, targetURL, nil, 1024, 1024)
		resultCh <- wsHandshakeResult{conn: conn, resp: resp, err: err}
	}()

	req, err := http.ReadRequest(bufio.NewReader(serverConn))
	if err != nil {
		t.Fatalf("read handshake request err: %v", err)
	}
	req.URL = targetURL
	req.Host = targetURL.Host

	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	serverWS, err := upgrader.Upgrade(&pipeResponseWriter{conn: serverConn}, req, nil)
	if err != nil {
		t.Fatalf("upgrade err: %v", err)
	}

	clientRes := <-resultCh
	if clientRes.err != nil {
		t.Fatalf("client handshake err: %v", clientRes.err)
	}

	t.Cleanup(func() {
		_ = clientRes.conn.Close()
		_ = serverWS.Close()
	})

	return clientRes.conn, serverWS
}

func newTestWsService(t *testing.T, conn *websocket.Conn, op *ConfOptions) (*WsService, context.CancelFunc) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	ws := &WsService{
		mu:        new(sync.Mutex),
		Logger:    log.New(io.Discard, "", 0),
		Ctx:       ctx,
		Client:    conn,
		once:      new(sync.Once),
		loginOnce: new(sync.Once),
		msgChs:    new(sync.Map),
		calls:     new(sync.Map),
		conf:      NewConnConfFromOption(op),
		status:    connected,
		clientMu:  new(sync.Mutex),
	}

	t.Cleanup(func() {
		ws.status = reconnecting
		cancel()
		if ws.Client != nil {
			_ = ws.Client.Close()
		}
	})

	return ws, cancel
}

func TestSubscribeWithoutCallback(t *testing.T) {
	clientConn, serverConn := newWebSocketPair(t)
	ws, cancel := newTestWsService(t, clientConn, &ConfOptions{
		URL:              "ws://example.com/ws",
		MaxRetryConn:     1,
		ShowReconnectMsg: false,
		PingInterval:     "24h",
	})
	defer cancel()

	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)

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
		if req.Channel != ChannelSpotPublicTrade || req.Event != Subscribe {
			t.Errorf("unexpected subscribe request: %+v", req)
			return
		}
		if payload, ok := req.Payload.([]any); !ok || len(payload) != 1 || payload[0] != "BTC_USDT" {
			t.Errorf("unexpected subscribe payload: %#v", req.Payload)
			return
		}

		rawResp, err := json.Marshal(UpdateMsg{
			Channel: ChannelSpotPublicTrade,
			Event:   "update",
			Result:  json.RawMessage(`[{"id":1,"create_time":1,"create_time_ms":"1","side":"buy","currency_pair":"BTC_USDT","amount":"1","price":"10"}]`),
		})
		if err != nil {
			t.Errorf("marshal update err: %v", err)
			return
		}
		if err := serverConn.WriteMessage(websocket.TextMessage, rawResp); err != nil {
			t.Errorf("write update err: %v", err)
			return
		}
	}()

	if err := ws.Subscribe(ChannelSpotPublicTrade, []string{"BTC_USDT"}); err != nil {
		t.Fatalf("Subscribe err: %v", err)
	}

	select {
	case <-serverDone:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for websocket exchange")
	}
}

func TestReadMsgSkipsInvalidJSONAndContinuesE2E(t *testing.T) {
	clientConn, serverConn := newWebSocketPair(t)
	ws, cancel := newTestWsService(t, clientConn, &ConfOptions{
		URL:              "ws://example.com/ws",
		MaxRetryConn:     1,
		ShowReconnectMsg: false,
		PingInterval:     "24h",
	})
	defer cancel()

	received := make(chan SpotTradeMsg, 1)
	ws.SetCallBack(ChannelSpotPublicTrade, NewCallBack(func(msg *UpdateMsg) {
		var trade SpotTradeMsg
		if err := json.Unmarshal(msg.Result, &trade); err != nil {
			t.Errorf("unmarshal trade update err: %v", err)
			return
		}
		received <- trade
	}))

	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
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
		if req.Channel != ChannelSpotPublicTrade {
			t.Errorf("unexpected subscribe channel: %+v", req)
			return
		}

		if err := serverConn.WriteMessage(websocket.TextMessage, []byte("{bad json")); err != nil {
			t.Errorf("write invalid json err: %v", err)
			return
		}

		goodResp, err := json.Marshal(UpdateMsg{
			Channel: ChannelSpotPublicTrade,
			Event:   "update",
			Result:  json.RawMessage(`{"id":77,"create_time":1,"create_time_ms":"1","side":"buy","currency_pair":"BTC_USDT","amount":"3","price":"11"}`),
		})
		if err != nil {
			t.Errorf("marshal good update err: %v", err)
			return
		}
		if err := serverConn.WriteMessage(websocket.TextMessage, goodResp); err != nil {
			t.Errorf("write good update err: %v", err)
			return
		}
	}()

	if err := ws.Subscribe(ChannelSpotPublicTrade, []string{"BTC_USDT"}); err != nil {
		t.Fatalf("Subscribe err: %v", err)
	}

	select {
	case trade := <-received:
		if trade.CurrencyPair != "BTC_USDT" || trade.Side != "buy" || trade.Amount != "3" {
			t.Fatalf("unexpected trade payload: %+v", trade)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for trade after invalid json")
	}

	select {
	case <-serverDone:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for invalid json exchange")
	}
}

func TestReadMsgStopsOnEmptyChannelE2E(t *testing.T) {
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
		if req.Channel != ChannelSpotPublicTrade {
			t.Errorf("unexpected subscribe channel: %+v", req)
			return
		}

		if err := serverConn.WriteMessage(websocket.TextMessage, []byte(`{"event":"update","result":{"id":1}}`)); err != nil {
			t.Errorf("write empty-channel update err: %v", err)
			return
		}

		time.Sleep(100 * time.Millisecond)
		_ = serverConn.WriteMessage(websocket.TextMessage, []byte(`{"channel":"spot.trades","event":"update","result":{"id":2}}`))
	}()

	if err := ws.Subscribe(ChannelSpotPublicTrade, []string{"BTC_USDT"}); err != nil {
		t.Fatalf("Subscribe err: %v", err)
	}

	select {
	case <-received:
		t.Fatal("expected empty channel message to stop reader before dispatching follow-up update")
	case <-time.After(500 * time.Millisecond):
	}

	select {
	case <-serverDone:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for empty-channel exchange")
	}
}

func TestReadMsgStopsOnContextCancelE2E(t *testing.T) {
	clientConn, serverConn := newWebSocketPair(t)
	ws, cancel := newTestWsService(t, clientConn, &ConfOptions{
		URL:              "ws://example.com/ws",
		MaxRetryConn:     1,
		ShowReconnectMsg: false,
		PingInterval:     "24h",
	})

	received := make(chan struct{}, 1)
	ws.SetCallBack(ChannelSpotPublicTrade, NewCallBack(func(msg *UpdateMsg) {
		received <- struct{}{}
	}))

	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
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
		if req.Channel != ChannelSpotPublicTrade {
			t.Errorf("unexpected subscribe channel: %+v", req)
			return
		}

		cancel()
		time.Sleep(50 * time.Millisecond)
		_ = serverConn.WriteMessage(websocket.TextMessage, []byte(`{"channel":"spot.trades","event":"update","result":{"id":3}}`))
	}()

	if err := ws.Subscribe(ChannelSpotPublicTrade, []string{"BTC_USDT"}); err != nil {
		t.Fatalf("Subscribe err: %v", err)
	}

	select {
	case <-received:
		t.Fatal("expected no callback after context cancel")
	case <-time.After(500 * time.Millisecond):
	}

	select {
	case <-serverDone:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for context-cancel exchange")
	}
}

func TestReadMsgReconnectFailureE2E(t *testing.T) {
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

	originalDial := dialWebSocket
	var dialCalls int32
	dialWebSocket = func(dialer *websocket.Dialer, url string, header http.Header) (*websocket.Conn, *http.Response, error) {
		atomic.AddInt32(&dialCalls, 1)
		return nil, nil, errors.New("dial failed")
	}
	t.Cleanup(func() {
		dialWebSocket = originalDial
	})

	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
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
		if req.Channel != ChannelSpotPublicTrade {
			t.Errorf("unexpected subscribe channel: %+v", req)
			return
		}
		_ = serverConn.Close()
	}()

	if err := ws.Subscribe(ChannelSpotPublicTrade, []string{"BTC_USDT"}); err != nil {
		t.Fatalf("Subscribe err: %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt32(&dialCalls) == 2 {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if got := atomic.LoadInt32(&dialCalls); got != 2 {
		t.Fatalf("unexpected reconnect dial calls: got %d want 2", got)
	}
	if got := ws.Status(); got != "reconnecting" {
		t.Fatalf("unexpected status after reconnect failure: got %q want %q", got, "reconnecting")
	}

	select {
	case <-received:
		t.Fatal("unexpected callback after reconnect failure")
	case <-time.After(500 * time.Millisecond):
	}

	select {
	case <-serverDone:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for reconnect-failure exchange")
	}
}

func TestGetConnectionAndStatusE2E(t *testing.T) {
	clientConn, _ := newWebSocketPair(t)
	ws, cancel := newTestWsService(t, clientConn, &ConfOptions{
		URL:              "ws://example.com/ws",
		MaxRetryConn:     1,
		ShowReconnectMsg: false,
		PingInterval:     "24h",
	})
	defer cancel()

	if ws.GetConnection() != clientConn {
		t.Fatal("GetConnection did not return the underlying websocket connection")
	}
	if got := ws.Status(); got != "connected" {
		t.Fatalf("unexpected status: got %q want %q", got, "connected")
	}
	ws.status = reconnecting
	if got := ws.Status(); got != "reconnecting" {
		t.Fatalf("unexpected reconnecting status: got %q want %q", got, "reconnecting")
	}
	ws.status = disconnected
	if got := ws.Status(); got != "disconnected" {
		t.Fatalf("unexpected disconnected status: got %q want %q", got, "disconnected")
	}
}

func TestSubscribeSpotOrdersE2E(t *testing.T) {
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

	received := make(chan SpotOrderMsg, 1)
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)

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
		if req.Channel != ChannelSpotOrder || req.Event != Subscribe {
			t.Errorf("unexpected subscribe request: %+v", req)
			return
		}

		rawResp, err := json.Marshal(UpdateMsg{
			Channel: ChannelSpotOrder,
			Event:   "update",
			Result: json.RawMessage(`[
				{
					"id":"123456",
					"text":"t-order",
					"create_time":"2026-04-29T00:00:00Z",
					"update_time":"2026-04-29T00:00:01Z",
					"currency_pair":"BTC_USDT",
					"type":"limit",
					"account":"spot",
					"side":"buy",
					"amount":"1",
					"price":"65000",
					"time_in_force":"gtc",
					"left":"0",
					"fill_price":"65000",
					"filled_total":"65000",
					"avg_deal_price":"65000",
					"fee":"0.1",
					"fee_currency":"USDT",
					"point_fee":"0",
					"gt_fee":"0",
					"gt_discount":false,
					"rebated_fee":"0",
					"rebated_fee_currency":"USDT",
					"stp_id":"98765",
					"stp_act":"cn",
					"finish_as":"filled",
					"biz_info":"biz",
					"amend_text":"amend"
				}
			]`),
		})
		if err != nil {
			t.Errorf("marshal update err: %v", err)
			return
		}
		if err := serverConn.WriteMessage(websocket.TextMessage, rawResp); err != nil {
			t.Errorf("write update err: %v", err)
			return
		}
	}()

	ws.SetCallBack(ChannelSpotOrder, NewCallBack(func(msg *UpdateMsg) {
		var orders []SpotOrderMsg
		if err := json.Unmarshal(msg.Result, &orders); err != nil {
			t.Errorf("unmarshal spot order update err: %v", err)
			return
		}
		if len(orders) == 0 {
			t.Errorf("expected at least one order in %q", string(msg.Result))
			return
		}
		received <- orders[0]
	}))

	if err := ws.Subscribe(ChannelSpotOrder, []string{"BTC_USDT"}); err != nil {
		t.Fatalf("Subscribe err: %v", err)
	}

	select {
	case order := <-received:
		if order.StpId != "98765" {
			t.Fatalf("unexpected stp id: got %q want %q", order.StpId, "98765")
		}
		if order.CurrencyPair != "BTC_USDT" || order.Side != "buy" || order.Amount != "1" {
			t.Fatalf("unexpected order payload: %+v", order)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for spot order callback")
	}

	select {
	case <-serverDone:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for websocket exchange")
	}
}

func TestSubscribeFuturesWithOptionsE2E(t *testing.T) {
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

	received := make(chan FuturesOrder, 1)
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)

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
		if req.Channel != ChannelFutureOrder || req.Event != Subscribe {
			t.Errorf("unexpected subscribe request: %+v", req)
			return
		}
		if req.Id == nil || *req.Id != 123456 {
			t.Errorf("unexpected subscribe id: %+v", req.Id)
			return
		}

		rawResp, err := json.Marshal(UpdateMsg{
			Channel: ChannelFutureOrder,
			Event:   "update",
			Result: json.RawMessage(`[
				{
					"id":123456,
					"user":"1001",
					"create_time":1,
					"create_time_ms":1,
					"finish_time":2,
					"finish_time_ms":2,
					"finish_as":"filled",
					"contract":"BTC_USDT",
					"size":1,
					"iceberg":0,
					"price":65000,
					"is_close":false,
					"is_reduce_only":false,
					"is_liq":false,
					"tif":"gtc",
					"left":0,
					"fill_price":65000,
					"text":"t-future",
					"tkfr":0,
					"mkfr":0,
					"refu":1,
					"refr":0,
					"stop_profit_price":"0",
					"stop_loss_price":"0",
					"stp_id":"555",
					"stp_act":"co",
					"biz_info":"biz",
					"amend_text":"amend"
				}
			]`),
		})
		if err != nil {
			t.Errorf("marshal update err: %v", err)
			return
		}
		if err := serverConn.WriteMessage(websocket.TextMessage, rawResp); err != nil {
			t.Errorf("write update err: %v", err)
			return
		}
	}()

	ws.SetCallBack(ChannelFutureOrder, NewCallBack(func(msg *UpdateMsg) {
		var orders []FuturesOrder
		if err := json.Unmarshal(msg.Result, &orders); err != nil {
			t.Errorf("unmarshal futures order update err: %v", err)
			return
		}
		if len(orders) == 0 {
			t.Errorf("expected at least one futures order in %q", string(msg.Result))
			return
		}
		received <- orders[0]
	}))

	if err := ws.SubscribeWithOption(ChannelFutureOrder, []string{"BTC_USDT"}, &SubscribeOptions{ID: 123456}); err != nil {
		t.Fatalf("SubscribeWithOption err: %v", err)
	}

	select {
	case order := <-received:
		if order.StpId != "555" {
			t.Fatalf("unexpected stp id: got %q want %q", order.StpId, "555")
		}
		if order.Contract != "BTC_USDT" || order.Size != 1 {
			t.Fatalf("unexpected futures order payload: %+v", order)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for futures order callback")
	}

	select {
	case <-serverDone:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for websocket exchange")
	}
}

func TestSubscribeAuthChannel(t *testing.T) {
	ws := &WsService{
		conf: &ConnConf{
			subscribeMsg: new(sync.Map),
		},
	}

	if err := ws.Subscribe(ChannelSpotOrder, []string{"BCH_USDT"}); err == nil {
		t.Fatal("expected auth error for auth channel without credentials")
	} else if err.Error() != "auth key or secret empty" {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestAPIRequestLoginAndAuthRequestsE2E(t *testing.T) {
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

	reqCh := make(chan Request, 3)
	done := make(chan struct{})
	go func() {
		defer close(done)

		for i := 0; i < 3; i++ {
			_, rawReq, err := serverConn.ReadMessage()
			if err != nil {
				t.Errorf("read request %d err: %v", i, err)
				return
			}

			var req Request
			if err := json.Unmarshal(rawReq, &req); err != nil {
				t.Errorf("unmarshal request %d err: %v", i, err)
				return
			}
			reqCh <- req
		}
	}()

	firstPayload := map[string]any{"currency_pair": "BTC_USDT"}
	if err := ws.APIRequest(ChannelSpotOrder, firstPayload, map[string]any{
		"req_id":            "req-1",
		"X-Gate-Channel-Id": "channel-1",
	}); err != nil {
		t.Fatalf("APIRequest #1 err: %v", err)
	}

	secondPayload := map[string]any{"currency_pair": "ETH_USDT"}
	if err := ws.APIRequest(ChannelSpotOrder, secondPayload, map[string]any{
		"req_id":            "req-2",
		"X-Gate-Channel-Id": "channel-2",
	}); err != nil {
		t.Fatalf("APIRequest #2 err: %v", err)
	}

	var got []Request
	for i := 0; i < 3; i++ {
		select {
		case req := <-reqCh:
			got = append(got, req)
		case <-time.After(3 * time.Second):
			t.Fatal("timed out waiting for auth request sequence")
		}
	}

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for auth request reader")
	}

	if len(got) != 3 {
		t.Fatalf("unexpected request count: got %d want 3", len(got))
	}

	checkAPIReq := func(t *testing.T, req Request, wantChannel, wantReqID, wantHeader string, wantParam string) {
		t.Helper()

		if req.Event != API {
			t.Fatalf("unexpected event: got %q want %q", req.Event, API)
		}
		if req.Channel != wantChannel {
			t.Fatalf("unexpected channel: got %q want %q", req.Channel, wantChannel)
		}

		payload, ok := req.Payload.(map[string]any)
		if !ok {
			t.Fatalf("unexpected payload type: %#v", req.Payload)
		}
		if payload["req_id"] != wantReqID {
			t.Fatalf("unexpected req_id: got %#v want %q", payload["req_id"], wantReqID)
		}
		if payload["timestamp"] == "" {
			t.Fatal("expected timestamp to be populated")
		}
		header, ok := payload["req_header"].(map[string]any)
		if !ok {
			t.Fatalf("unexpected req_header type: %#v", payload["req_header"])
		}
		if header["X-Gate-Channel-Id"] != wantHeader {
			t.Fatalf("unexpected X-Gate-Channel-Id: got %#v want %q", header["X-Gate-Channel-Id"], wantHeader)
		}
		param, _ := payload["req_param"].(map[string]any)
		if wantParam == "" {
			if len(param) != 0 {
				t.Fatalf("unexpected req_param: %#v", payload["req_param"])
			}
		} else if param["currency_pair"] != wantParam {
			t.Fatalf("unexpected req_param currency_pair: got %#v want %q", param["currency_pair"], wantParam)
		}
	}

	checkLoginReq := func(t *testing.T, req Request, wantReqID, wantHeader string) {
		t.Helper()

		if req.Channel != ChannelSpotLogin {
			t.Fatalf("unexpected login channel: got %q want %q", req.Channel, ChannelSpotLogin)
		}
		if req.Event != API {
			t.Fatalf("unexpected login event: got %q want %q", req.Event, API)
		}
		payload, ok := req.Payload.(map[string]any)
		if !ok {
			t.Fatalf("unexpected login payload type: %#v", req.Payload)
		}
		if payload["req_id"] != wantReqID {
			t.Fatalf("unexpected login req_id: got %#v want %q", payload["req_id"], wantReqID)
		}
		header, ok := payload["req_header"].(map[string]any)
		if !ok {
			t.Fatalf("unexpected login req_header type: %#v", payload["req_header"])
		}
		if header["X-Gate-Channel-Id"] != wantHeader {
			t.Fatalf("unexpected login X-Gate-Channel-Id: got %#v want %q", header["X-Gate-Channel-Id"], wantHeader)
		}
		if payload["req_param"] != nil {
			t.Fatalf("unexpected login req_param: %#v", payload["req_param"])
		}
	}

	checkLoginReq(t, got[0], "req_id", "T_channel_id")
	checkAPIReq(t, got[1], ChannelSpotOrder, "req-1", "channel-1", "BTC_USDT")
	checkAPIReq(t, got[2], ChannelSpotOrder, "req-2", "channel-2", "ETH_USDT")
}

func TestAPIRequestLoginAndAuthRequestsFuturesE2E(t *testing.T) {
	clientConn, serverConn := newWebSocketPair(t)
	ws, cancel := newTestWsService(t, clientConn, &ConfOptions{
		App:              "futures",
		URL:              "ws://example.com/ws",
		Key:              "test-key",
		Secret:           "test-secret",
		MaxRetryConn:     1,
		ShowReconnectMsg: false,
		PingInterval:     "24h",
	})
	defer cancel()

	reqCh := make(chan Request, 3)
	done := make(chan struct{})
	go func() {
		defer close(done)

		for i := 0; i < 3; i++ {
			_, rawReq, err := serverConn.ReadMessage()
			if err != nil {
				t.Errorf("read request %d err: %v", i, err)
				return
			}

			var req Request
			if err := json.Unmarshal(rawReq, &req); err != nil {
				t.Errorf("unmarshal request %d err: %v", i, err)
				return
			}
			reqCh <- req
		}
	}()

	if err := ws.APIRequest(ChannelFutureOrder, map[string]any{"contract": "BTC_USDT"}, map[string]any{
		"req_id":            "req-f1",
		"X-Gate-Channel-Id": "future-channel-1",
	}); err != nil {
		t.Fatalf("APIRequest #1 err: %v", err)
	}

	if err := ws.APIRequest(ChannelFutureOrder, map[string]any{"contract": "ETH_USDT"}, map[string]any{
		"req_id":            "req-f2",
		"X-Gate-Channel-Id": "future-channel-2",
	}); err != nil {
		t.Fatalf("APIRequest #2 err: %v", err)
	}

	var got []Request
	for i := 0; i < 3; i++ {
		select {
		case req := <-reqCh:
			got = append(got, req)
		case <-time.After(3 * time.Second):
			t.Fatal("timed out waiting for futures auth request sequence")
		}
	}

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for futures auth request reader")
	}

	if len(got) != 3 {
		t.Fatalf("unexpected request count: got %d want 3", len(got))
	}

	checkLoginReq := func(t *testing.T, req Request, wantChannel, wantReqID, wantHeader string) {
		t.Helper()

		if req.Channel != wantChannel {
			t.Fatalf("unexpected login channel: got %q want %q", req.Channel, wantChannel)
		}
		if req.Event != API {
			t.Fatalf("unexpected login event: got %q want %q", req.Event, API)
		}
		payload, ok := req.Payload.(map[string]any)
		if !ok {
			t.Fatalf("unexpected login payload type: %#v", req.Payload)
		}
		if payload["req_id"] != wantReqID {
			t.Fatalf("unexpected login req_id: got %#v want %q", payload["req_id"], wantReqID)
		}
		header, ok := payload["req_header"].(map[string]any)
		if !ok {
			t.Fatalf("unexpected login req_header type: %#v", payload["req_header"])
		}
		if header["X-Gate-Channel-Id"] != wantHeader {
			t.Fatalf("unexpected login X-Gate-Channel-Id: got %#v want %q", header["X-Gate-Channel-Id"], wantHeader)
		}
		if payload["req_param"] != nil {
			t.Fatalf("unexpected login req_param: %#v", payload["req_param"])
		}
	}

	checkAPIReq := func(t *testing.T, req Request, wantReqID, wantHeader, wantContract string) {
		t.Helper()

		if req.Channel != ChannelFutureOrder {
			t.Fatalf("unexpected channel: got %q want %q", req.Channel, ChannelFutureOrder)
		}
		if req.Event != API {
			t.Fatalf("unexpected event: got %q want %q", req.Event, API)
		}

		payload, ok := req.Payload.(map[string]any)
		if !ok {
			t.Fatalf("unexpected payload type: %#v", req.Payload)
		}
		if payload["req_id"] != wantReqID {
			t.Fatalf("unexpected req_id: got %#v want %q", payload["req_id"], wantReqID)
		}
		if payload["timestamp"] == "" {
			t.Fatal("expected timestamp to be populated")
		}
		header, ok := payload["req_header"].(map[string]any)
		if !ok {
			t.Fatalf("unexpected req_header type: %#v", payload["req_header"])
		}
		if header["X-Gate-Channel-Id"] != wantHeader {
			t.Fatalf("unexpected X-Gate-Channel-Id: got %#v want %q", header["X-Gate-Channel-Id"], wantHeader)
		}
		param, _ := payload["req_param"].(map[string]any)
		if param["contract"] != wantContract {
			t.Fatalf("unexpected req_param contract: got %#v want %q", param["contract"], wantContract)
		}
	}

	checkLoginReq(t, got[0], ChannelFutureLogin, "req_id", "T_channel_id")
	checkAPIReq(t, got[1], "req-f1", "future-channel-1", "BTC_USDT")
	checkAPIReq(t, got[2], "req-f2", "future-channel-2", "ETH_USDT")
}

func TestUnSubscribeUsesUnsubscribeSignature(t *testing.T) {
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
			t.Errorf("read unsubscribe request err: %v", err)
			return
		}

		var req Request
		if err := json.Unmarshal(rawReq, &req); err != nil {
			t.Errorf("unmarshal unsubscribe request err: %v", err)
			return
		}
		if req.Event != UnSubscribe {
			t.Fatalf("unexpected event: got %q want %q", req.Event, UnSubscribe)
		}

		ts := req.Time
		hash := hmac.New(sha512.New, []byte("test-secret"))
		hash.Write([]byte("channel=spot.trades&event=unsubscribe&time=" + strconv.FormatInt(ts, 10)))
		want := hex.EncodeToString(hash.Sum(nil))
		if req.Auth.Secret != want {
			t.Fatalf("unexpected auth secret: got %q want %q", req.Auth.Secret, want)
		}
	}()

	if err := ws.UnSubscribe(ChannelSpotPublicTrade, []string{"BTC_USDT"}); err != nil {
		t.Fatalf("UnSubscribe err: %v", err)
	}

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for unsubscribe request")
	}
}

func TestActivePingSendsPingFrameE2E(t *testing.T) {
	clientConn, serverConn := newWebSocketPair(t)
	ws, cancel := newTestWsService(t, clientConn, &ConfOptions{
		URL:              "ws://example.com/ws",
		MaxRetryConn:     1,
		ShowReconnectMsg: false,
		PingInterval:     "10ms",
	})
	defer cancel()

	ws.conf.subscribeMsg.Store(ChannelSpotPublicTrade, []requestHistory{
		{
			Channel: ChannelSpotPublicTrade,
			Event:   Subscribe,
			Payload: []string{"BTC_USDT"},
		},
	})

	pingSeen := make(chan struct{}, 1)
	go func() {
		ws.activePing()
	}()
	go func() {
		_, rawReq, err := serverConn.ReadMessage()
		if err != nil {
			t.Errorf("read ping request err: %v", err)
			return
		}

		var req Request
		if err := json.Unmarshal(rawReq, &req); err != nil {
			t.Errorf("unmarshal ping request err: %v", err)
			return
		}
		if req.Channel != "spot.ping" || req.Event != Subscribe {
			t.Errorf("unexpected ping request: %+v", req)
			return
		}
		pingSeen <- struct{}{}
	}()

	select {
	case <-pingSeen:
		cancel()
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for active ping frame")
	}
}

func TestReconnectResubscribesChannelsE2E(t *testing.T) {
	oldClientConn, _ := newWebSocketPair(t)
	newClientConn, newServerConn := newWebSocketPair(t)

	originalDial := dialWebSocket
	dialCalls := 0
	dialWebSocket = func(dialer *websocket.Dialer, url string, header http.Header) (*websocket.Conn, *http.Response, error) {
		dialCalls++
		if dialCalls == 1 {
			return nil, nil, errors.New("dial failed")
		}
		return newClientConn, nil, nil
	}
	t.Cleanup(func() {
		dialWebSocket = originalDial
	})

	ws, cancel := newTestWsService(t, oldClientConn, &ConfOptions{
		URL:              "ws://example.com/ws",
		Key:              "test-key",
		Secret:           "test-secret",
		MaxRetryConn:     1,
		ShowReconnectMsg: false,
		PingInterval:     "24h",
	})
	defer cancel()

	ws.conf.subscribeMsg.Store(ChannelSpotOrder, []requestHistory{
		{
			Channel: ChannelSpotOrder,
			Event:   Subscribe,
			Payload: []string{"BTC_USDT"},
		},
	})
	ws.conf.subscribeMsg.Store(ChannelFutureOrder, []requestHistory{
		{
			Channel: ChannelFutureOrder,
			Event:   Subscribe,
			Payload: []string{"BTC_USDT"},
		},
	})

	recv := make(chan Request, 2)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 2; i++ {
			_, rawReq, err := newServerConn.ReadMessage()
			if err != nil {
				t.Errorf("read reconnect request %d err: %v", i, err)
				return
			}

			var req Request
			if err := json.Unmarshal(rawReq, &req); err != nil {
				t.Errorf("unmarshal reconnect request %d err: %v", i, err)
				return
			}
			recv <- req
		}
	}()

	if err := ws.reconnect(); err != nil {
		t.Fatalf("reconnect err: %v", err)
	}
	if ws.GetConnection() != newClientConn {
		t.Fatal("reconnect did not replace websocket connection")
	}
	if got := ws.Status(); got != "connected" {
		t.Fatalf("unexpected status after reconnect: got %q want %q", got, "connected")
	}

	got := make([]Request, 0, 2)
	for i := 0; i < 2; i++ {
		select {
		case req := <-recv:
			got = append(got, req)
		case <-time.After(3 * time.Second):
			t.Fatal("timed out waiting for reconnect resubscribe requests")
		}
	}

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for reconnect reader")
	}

	if len(got) != 2 {
		t.Fatalf("unexpected reconnect request count: got %d want 2", len(got))
	}

	for _, req := range got {
		if req.Event != Subscribe {
			t.Fatalf("unexpected reconnect event: %+v", req)
		}
		if req.Channel != ChannelSpotOrder && req.Channel != ChannelFutureOrder {
			t.Fatalf("unexpected reconnect channel: %+v", req)
		}
		payload, ok := req.Payload.([]any)
		if !ok || len(payload) != 1 || payload[0] != "BTC_USDT" {
			t.Fatalf("unexpected reconnect payload: %#v", req.Payload)
		}
	}
}

func TestNewWsServiceRetriesAndConnectsE2E(t *testing.T) {
	clientConn, _ := newWebSocketPair(t)

	originalDial := dialWebSocket
	dialCalls := 0
	dialWebSocket = func(dialer *websocket.Dialer, url string, header http.Header) (*websocket.Conn, *http.Response, error) {
		dialCalls++
		if dialCalls < 3 {
			return nil, nil, errors.New("dial failed")
		}
		return clientConn, nil, nil
	}
	t.Cleanup(func() {
		dialWebSocket = originalDial
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ws, err := NewWsService(ctx, log.New(io.Discard, "", 0), &ConnConf{
		URL:              "ws://example.com/ws",
		MaxRetryConn:     3,
		ShowReconnectMsg: false,
		PingInterval:     "24h",
		subscribeMsg:     new(sync.Map),
	})
	if err != nil {
		t.Fatalf("NewWsService err: %v", err)
	}
	defer func() {
		if ws != nil && ws.Client != nil {
			_ = ws.Client.Close()
		}
	}()

	if dialCalls != 3 {
		t.Fatalf("unexpected dial calls: got %d want 3", dialCalls)
	}
	if ws.GetConnection() != clientConn {
		t.Fatal("NewWsService did not store the connected websocket")
	}
	if got := ws.Status(); got != "connected" {
		t.Fatalf("unexpected status: got %q want %q", got, "connected")
	}
}

func TestNewWsServiceRetriesAndFailsE2E(t *testing.T) {
	originalDial := dialWebSocket
	dialCalls := 0
	dialWebSocket = func(dialer *websocket.Dialer, url string, header http.Header) (*websocket.Conn, *http.Response, error) {
		dialCalls++
		return nil, nil, errors.New("dial failed")
	}
	t.Cleanup(func() {
		dialWebSocket = originalDial
	})

	ws, err := NewWsService(nil, log.New(io.Discard, "", 0), &ConnConf{
		URL:              "ws://example.com/ws",
		MaxRetryConn:     1,
		ShowReconnectMsg: false,
		PingInterval:     "24h",
		subscribeMsg:     new(sync.Map),
	})
	if err == nil {
		t.Fatal("expected NewWsService to fail after retries are exhausted")
	}
	if ws != nil {
		t.Fatalf("expected nil websocket service on failure, got %+v", ws)
	}
	if dialCalls != 2 {
		t.Fatalf("unexpected dial calls: got %d want 2", dialCalls)
	}
}

func TestReconnectRetriesAndFailsE2E(t *testing.T) {
	clientConn, _ := newWebSocketPair(t)
	ws, cancel := newTestWsService(t, clientConn, &ConfOptions{
		URL:              "ws://example.com/ws",
		Key:              "test-key",
		Secret:           "test-secret",
		MaxRetryConn:     1,
		ShowReconnectMsg: false,
		PingInterval:     "24h",
	})
	defer cancel()

	originalDial := dialWebSocket
	dialCalls := 0
	dialWebSocket = func(dialer *websocket.Dialer, url string, header http.Header) (*websocket.Conn, *http.Response, error) {
		dialCalls++
		return nil, nil, errors.New("dial failed")
	}
	t.Cleanup(func() {
		dialWebSocket = originalDial
	})

	err := ws.reconnect()
	if err == nil {
		t.Fatal("expected reconnect to fail after retries are exhausted")
	}
	if dialCalls != 2 {
		t.Fatalf("unexpected dial calls: got %d want 2", dialCalls)
	}
	if got := ws.Status(); got != "reconnecting" {
		t.Fatalf("unexpected status after reconnect failure: got %q want %q", got, "reconnecting")
	}
}
