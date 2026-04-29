package gatews

import (
	"sync"
	"testing"
)

func TestGetChannelMarkets(t *testing.T) {
	ws := &WsService{
		conf: &ConnConf{
			subscribeMsg: new(sync.Map),
		},
	}

	ws.conf.subscribeMsg.Store(ChannelSpotPublicTrade, []requestHistory{
		{
			Channel: ChannelSpotPublicTrade,
			Event:   Subscribe,
			Payload: []string{"BCH_USDT", "BTC_USDT", "ignored"},
		},
		{
			Channel: ChannelSpotPublicTrade,
			Event:   UnSubscribe,
			Payload: []string{"BTC_USDT"},
		},
		{
			Channel: ChannelSpotPublicTrade,
			Event:   Subscribe,
			Payload: []string{"ETH_USDT"},
		},
	})

	got := ws.GetChannelMarkets(ChannelSpotPublicTrade)
	want := map[string]struct{}{
		"BCH_USDT": {},
		"ETH_USDT": {},
	}

	if len(got) != len(want) {
		t.Fatalf("unexpected markets length: got %d want %d (%v)", len(got), len(want), got)
	}
	for _, market := range got {
		if _, ok := want[market]; !ok {
			t.Fatalf("unexpected market %q in %v", market, got)
		}
	}
}

func TestGetChannels(t *testing.T) {
	ws := &WsService{
		calls: new(sync.Map),
	}

	ws.calls.Store(ChannelSpotPublicTrade, CallBack(func(*UpdateMsg) {}))
	ws.calls.Store(ChannelSpotCandleStick, CallBack(func(*UpdateMsg) {}))

	got := ws.GetChannels()
	if len(got) != 2 {
		t.Fatalf("unexpected channels length: got %d want 2 (%v)", len(got), got)
	}

	want := map[string]struct{}{
		ChannelSpotPublicTrade: {},
		ChannelSpotCandleStick: {},
	}
	for _, channel := range got {
		if _, ok := want[channel]; !ok {
			t.Fatalf("unexpected channel %q in %v", channel, got)
		}
	}
}

func TestGetConfAndOptions(t *testing.T) {
	conf := NewConnConfFromOption(&ConfOptions{
		URL:              "",
		Key:              "KEY",
		Secret:           "SECRET",
		MaxRetryConn:     10,
		PingInterval:     "12s",
		ShowReconnectMsg: false,
	})

	if conf.URL != BaseUrl {
		t.Fatalf("unexpected default url: got %q want %q", conf.URL, BaseUrl)
	}
	if conf.MaxRetryConn != 10 {
		t.Fatalf("unexpected max retry: got %d want 10", conf.MaxRetryConn)
	}
	if conf.Key != "KEY" || conf.Secret != "SECRET" {
		t.Fatalf("unexpected key/secret: %+v", conf)
	}

	ws := &WsService{conf: conf}
	if got := ws.GetKey(); got != "KEY" {
		t.Fatalf("unexpected key: got %q want %q", got, "KEY")
	}
	if got := ws.GetSecret(); got != "SECRET" {
		t.Fatalf("unexpected secret: got %q want %q", got, "SECRET")
	}
	if got := ws.GetMaxRetryConn(); got != 10 {
		t.Fatalf("unexpected max retry: got %d want 10", got)
	}

	ws.SetKey("KEY2")
	ws.SetSecret("SECRET2")
	ws.SetMaxRetryConn(20)
	if got := ws.GetKey(); got != "KEY2" {
		t.Fatalf("unexpected updated key: got %q want %q", got, "KEY2")
	}
	if got := ws.GetSecret(); got != "SECRET2" {
		t.Fatalf("unexpected updated secret: got %q want %q", got, "SECRET2")
	}
	if got := ws.GetMaxRetryConn(); got != 20 {
		t.Fatalf("unexpected updated max retry: got %d want 20", got)
	}
}

func TestNewConnConfFromOptionNil(t *testing.T) {
	conf := NewConnConfFromOption(nil)
	if conf == nil {
		t.Fatal("expected conf to be initialized")
	}
	if conf.URL != BaseUrl {
		t.Fatalf("unexpected default url: got %q want %q", conf.URL, BaseUrl)
	}
	if conf.MaxRetryConn != MaxRetryConn {
		t.Fatalf("unexpected default max retry: got %d want %d", conf.MaxRetryConn, MaxRetryConn)
	}
	if conf.subscribeMsg == nil {
		t.Fatal("expected subscribeMsg map to be initialized")
	}
}
