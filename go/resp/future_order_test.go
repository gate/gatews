package resp

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestFutureOrderStpIDJSONRoundTrip(t *testing.T) {
	src := `{"contract":"BTC_USDT","size":1,"stp_id":"13579","stp_act":"cb"}`

	var order FutureOrder
	if err := json.Unmarshal([]byte(src), &order); err != nil {
		t.Fatalf("unmarshal err: %v", err)
	}
	if order.StpId != "13579" {
		t.Fatalf("unexpected stp id: got %q want %q", order.StpId, "13579")
	}
	if order.StpAct != "cb" {
		t.Fatalf("unexpected stp act: got %q want %q", order.StpAct, "cb")
	}

	data, err := json.Marshal(order)
	if err != nil {
		t.Fatalf("marshal err: %v", err)
	}
	if !strings.Contains(string(data), `"stp_id":"13579"`) {
		t.Fatalf("marshal output missing stp_id: %s", string(data))
	}
}
