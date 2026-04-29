package model

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestFuturesOrderStpIDJSONRoundTrip(t *testing.T) {
	src := `{"contract":"BTC_USDT","size":1,"stp_id":"98765","stp_act":"cn"}`

	var order FuturesOrder
	if err := json.Unmarshal([]byte(src), &order); err != nil {
		t.Fatalf("unmarshal err: %v", err)
	}
	if order.StpId != "98765" {
		t.Fatalf("unexpected stp id: got %q want %q", order.StpId, "98765")
	}
	if order.StpAct != "cn" {
		t.Fatalf("unexpected stp act: got %q want %q", order.StpAct, "cn")
	}

	data, err := json.Marshal(order)
	if err != nil {
		t.Fatalf("marshal err: %v", err)
	}
	if !strings.Contains(string(data), `"stp_id":"98765"`) {
		t.Fatalf("marshal output missing stp_id: %s", string(data))
	}
}
