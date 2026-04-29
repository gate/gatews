package resp

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSpotOrderStpIDJSONRoundTrip(t *testing.T) {
	src := `{"id":"123456","currency_pair":"BTC_USDT","side":"sell","amount":"2","price":"100","stp_id":"24680","stp_act":"cn"}`

	var order SpotOrder
	if err := json.Unmarshal([]byte(src), &order); err != nil {
		t.Fatalf("unmarshal err: %v", err)
	}
	if order.StpId != "24680" {
		t.Fatalf("unexpected stp id: got %q want %q", order.StpId, "24680")
	}
	if order.StpAct != "cn" {
		t.Fatalf("unexpected stp act: got %q want %q", order.StpAct, "cn")
	}

	data, err := json.Marshal(order)
	if err != nil {
		t.Fatalf("marshal err: %v", err)
	}
	if !strings.Contains(string(data), `"stp_id":"24680"`) {
		t.Fatalf("marshal output missing stp_id: %s", string(data))
	}
}
