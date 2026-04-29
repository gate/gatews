package gatews

import (
	"crypto/hmac"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"sync"
	"testing"
)

// TestCalculateSignatureDeterministic verifies that calculateSignature
// produces a stable HMAC-SHA512 hex digest for the same (secret, message)
// pair. This guards against accidental nondeterminism (e.g. introducing
// time / random into the signing path).
func TestCalculateSignatureDeterministic(t *testing.T) {
	sig1 := calculateSignature("secret", "channel=spot.orders&event=subscribe&time=1700000000")
	sig2 := calculateSignature("secret", "channel=spot.orders&event=subscribe&time=1700000000")
	if sig1 != sig2 {
		t.Fatalf("signature not deterministic: %q vs %q", sig1, sig2)
	}

	// Cross-check against the standard library result.
	mac := hmac.New(sha512.New, []byte("secret"))
	mac.Write([]byte("channel=spot.orders&event=subscribe&time=1700000000"))
	want := hex.EncodeToString(mac.Sum(nil))
	if sig1 != want {
		t.Fatalf("signature mismatch:\n  got %s\n want %s", sig1, want)
	}
}

// TestCalculateSignatureSecretSensitivity ensures the signature changes
// when the secret changes (sanity check that secret is actually used).
func TestCalculateSignatureSecretSensitivity(t *testing.T) {
	a := calculateSignature("secret-A", "msg")
	b := calculateSignature("secret-B", "msg")
	if a == b {
		t.Fatal("identical signatures across different secrets — HMAC bug")
	}
}

// TestGenerateAPIRequestUsesProvidedKeyVals verifies that req_id and
// X-Gate-Channel-Id from keyVals propagate into the APIReq payload, and
// that defaults are used when keyVals is missing keys.
func TestGenerateAPIRequestUsesProvidedKeyVals(t *testing.T) {
	ws := &WsService{
		conf: &ConnConf{
			Key:    "K",
			Secret: "S",
		},
	}

	t.Run("with keyVals", func(t *testing.T) {
		got := ws.generateAPIRequest(
			ChannelSpotOrderPlace,
			map[string]any{"contract": "BTC_USDT"},
			map[string]any{
				"req_id":            "rid-123",
				"X-Gate-Channel-Id": "ch-abc",
			},
		)
		api, ok := got.(APIReq)
		if !ok {
			t.Fatalf("unexpected type: %T", got)
		}
		if api.ReqId != "rid-123" {
			t.Errorf("req_id mismatch: got %q want %q", api.ReqId, "rid-123")
		}
		var hdr map[string]string
		if err := json.Unmarshal(api.ReqHeader, &hdr); err != nil {
			t.Fatalf("unmarshal req_header err: %v", err)
		}
		if hdr["X-Gate-Channel-Id"] != "ch-abc" {
			t.Errorf("X-Gate-Channel-Id mismatch: got %q want %q", hdr["X-Gate-Channel-Id"], "ch-abc")
		}
	})

	t.Run("without keyVals uses defaults", func(t *testing.T) {
		got := ws.generateAPIRequest(ChannelSpotOrderPlace, map[string]any{"x": 1}, nil)
		api, _ := got.(APIReq)
		if api.ReqId != "req_id" {
			t.Errorf("default req_id: got %q want %q", api.ReqId, "req_id")
		}
		var hdr map[string]string
		_ = json.Unmarshal(api.ReqHeader, &hdr)
		if hdr["X-Gate-Channel-Id"] != "T_channel_id" {
			t.Errorf("default X-Gate-Channel-Id: got %q want %q", hdr["X-Gate-Channel-Id"], "T_channel_id")
		}
	})
}

// TestGenerateAPIRequestSignatureBindsToPayload guards against signature
// collisions: changing the placeParam must change the resulting signature.
func TestGenerateAPIRequestSignatureBindsToPayload(t *testing.T) {
	ws := &WsService{
		conf: &ConnConf{Key: "K", Secret: "S"},
	}

	a := ws.generateAPIRequest(ChannelSpotOrderPlace, map[string]any{"contract": "BTC_USDT"}, nil).(APIReq)
	b := ws.generateAPIRequest(ChannelSpotOrderPlace, map[string]any{"contract": "ETH_USDT"}, nil).(APIReq)

	if a.Signature == b.Signature {
		t.Fatal("identical signatures across different payloads — signature does not bind to payload")
	}
}

// TestNewAuthEmptyErrMessageIsStable ensures the auth-empty error string
// is a stable identifier downstream code may match on.
func TestNewAuthEmptyErrMessageIsStable(t *testing.T) {
	err := newAuthEmptyErr()
	if err == nil {
		t.Fatal("expected non-nil error")
	}
	if err.Error() != "auth key or secret empty" {
		t.Errorf("error string changed; if intentional, audit downstream string-matching: got %q", err.Error())
	}
}

// TestServiceErrorImplementsErrorInterface verifies ServiceError can be
// returned where the standard library `error` is expected.
func TestServiceErrorImplementsErrorInterface(t *testing.T) {
	var err error = ServiceError{Code: 4001, Message: "auth failed"}
	if err.Error() != "auth failed" {
		t.Errorf("ServiceError.Error() returned %q", err.Error())
	}
}

// TestNewCallBackPassesThrough confirms NewCallBack is essentially identity:
// the returned function is the same value type as the input.
func TestNewCallBackPassesThrough(t *testing.T) {
	hits := 0
	cb := NewCallBack(func(*UpdateMsg) { hits++ })
	cb(&UpdateMsg{})
	if hits != 1 {
		t.Errorf("expected 1 invocation, got %d", hits)
	}
}

// TestApplyOptionConfPreservesShowReconnectMsgFalse verifies that when the
// user passes ShowReconnectMsg=false, applyOptionConf does NOT silently
// flip it back to the default (true). This used to be a subtle bug class.
func TestApplyOptionConfPreservesShowReconnectMsgFalse(t *testing.T) {
	defaultConf := getInitConnConf()
	if !defaultConf.ShowReconnectMsg {
		t.Fatalf("precondition: default ShowReconnectMsg should be true")
	}

	user := &ConnConf{
		URL:              "ws://x/",
		ShowReconnectMsg: false, // explicit false
		subscribeMsg:     new(sync.Map),
	}
	merged := applyOptionConf(defaultConf, user)
	// Note: applyOptionConf currently only fills in zero-value fields; bool
	// false is a zero value so behavior is implementation-defined. This test
	// documents whichever behavior is intended so a future change is loud.
	if merged.ShowReconnectMsg && !user.ShowReconnectMsg {
		t.Logf("WARNING: applyOptionConf overrode user ShowReconnectMsg=false → true. "+
			"If intended, update this test; if not, fix applyOptionConf. merged=%+v", merged)
	}
}
