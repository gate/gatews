package model

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSpotOrderStpIDJSONRoundTrip(t *testing.T) {
	src := `{"id":"123456","currency_pair":"BTC_USDT","side":"buy","amount":"1","price":"65000","stp_id":"0","stp_act":"co"}`

	var order Order
	if err := json.Unmarshal([]byte(src), &order); err != nil {
		t.Fatalf("unmarshal err: %v", err)
	}
	if order.StpId != "0" {
		t.Fatalf("unexpected stp id: got %q want %q", order.StpId, "0")
	}
	if order.StpAct != "co" {
		t.Fatalf("unexpected stp act: got %q want %q", order.StpAct, "co")
	}

	data, err := json.Marshal(order)
	if err != nil {
		t.Fatalf("marshal err: %v", err)
	}
	if !strings.Contains(string(data), `"stp_id":"0"`) {
		t.Fatalf("marshal output missing stp_id: %s", string(data))
	}
}
