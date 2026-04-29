package gatews

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestResponseSpotOrderMsgStpIDJSONRoundTrip(t *testing.T) {
	src := `{"id":"123456","currency_pair":"BTC_USDT","side":"buy","amount":"1","price":"65000","stp_id":"24680","stp_act":"cb"}`

	var order OrderMsg
	if err := json.Unmarshal([]byte(src), &order); err != nil {
		t.Fatalf("unmarshal err: %v", err)
	}
	if order.StpId != "24680" {
		t.Fatalf("unexpected stp id: got %q want %q", order.StpId, "24680")
	}
	if order.StpAct != "cb" {
		t.Fatalf("unexpected stp act: got %q want %q", order.StpAct, "cb")
	}

	data, err := json.Marshal(order)
	if err != nil {
		t.Fatalf("marshal err: %v", err)
	}
	if !strings.Contains(string(data), `"stp_id":"24680"`) {
		t.Fatalf("marshal output missing stp_id: %s", string(data))
	}
}

func TestResponseFuturesOrderStpIDJSONRoundTrip(t *testing.T) {
	src := `{"contract":"BTC_USDT","size":1,"stp_id":"13579","stp_act":"co"}`

	var order FuturesOrder
	if err := json.Unmarshal([]byte(src), &order); err != nil {
		t.Fatalf("unmarshal err: %v", err)
	}
	if order.StpId != "13579" {
		t.Fatalf("unexpected stp id: got %q want %q", order.StpId, "13579")
	}
	if order.StpAct != "co" {
		t.Fatalf("unexpected stp act: got %q want %q", order.StpAct, "co")
	}

	data, err := json.Marshal(order)
	if err != nil {
		t.Fatalf("marshal err: %v", err)
	}
	if !strings.Contains(string(data), `"stp_id":"13579"`) {
		t.Fatalf("marshal output missing stp_id: %s", string(data))
	}
}

func TestUpdateMsgGetChannelPrefersBodyChannel(t *testing.T) {
	msg := UpdateMsg{
		Channel: "body.channel",
		Header: ResponseHeader{
			Channel: "header.channel",
		},
	}
	if got := msg.GetChannel(); got != "body.channel" {
		t.Fatalf("unexpected channel: got %q want %q", got, "body.channel")
	}
}

func TestUpdateMsgGetChannelFallsBackToHeader(t *testing.T) {
	msg := UpdateMsg{
		Header: ResponseHeader{
			Channel: "header.channel",
		},
	}
	if got := msg.GetChannel(); got != "header.channel" {
		t.Fatalf("unexpected channel: got %q want %q", got, "header.channel")
	}
}
