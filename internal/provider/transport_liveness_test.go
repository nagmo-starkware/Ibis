package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/NethermindEth/juno/core/felt"
)

// startLivenessSub starts a subscriber on the given transport with two
// contracts (one option-family, one static token) whose start block is within
// catchupThreshold of the tip, so each stream can reach "live" immediately.
func startLivenessSub(t *testing.T, cfg *SubscriberConfig, tip uint64, startBlock uint64) (*EventSubscriber, func()) {
	t.Helper()
	handlers := map[string]func(json.RawMessage) (interface{}, error){
		"starknet_blockNumber": func(_ json.RawMessage) (interface{}, error) { return tip, nil },
		"starknet_getEvents": func(_ json.RawMessage) (interface{}, error) {
			return map[string]interface{}{"events": []interface{}{}}, nil
		},
	}
	server := mockRPCServer(t, handlers)
	p, err := New(context.Background(), server.URL, nil)
	if err != nil {
		server.Close()
		t.Fatalf("New() error: %v", err)
	}
	sub := p.NewSubscriber(
		[]ContractSubscription{
			{Address: newTestFelt(0xA), StartBlock: startBlock, Wildcard: true},
			{Address: newTestFelt(0xB), StartBlock: startBlock, Wildcard: false},
		},
		make(chan RawEvent, 64),
		cfg,
	)
	sub.dialWSS = mockWSSDialerKeyed(nil, nil)

	ctx, cancel := context.WithCancel(context.Background())
	go sub.Start(ctx)
	return sub, func() { cancel(); p.Close(); server.Close() }
}

// TestLivenessEveryTransport: catchup_complete has to mean the same thing on
// every transport. It used to be counted only by the firehose-keys transport, so
// on any other it stayed false forever — and IBIS_TRANSPORT is the documented
// instant-rollback lever, so flipping it during an incident would have jammed
// every promote exactly when promotes matter most.
func TestLivenessEveryTransport(t *testing.T) {
	cases := []struct {
		name      string
		cfg       *SubscriberConfig
		wantTotal int64
	}{
		{"firehose-keys", &SubscriberConfig{KeysFirehose: true, OptionSelectors: []*felt.Felt{newTestFelt(0x999)}}, 2},
		{"shared firehose (one stream)", &SubscriberConfig{SharedFirehose: true}, 1},
		{"per-contract WSS", &SubscriberConfig{}, 2},
		{"per-contract, catch up then WSS", &SubscriberConfig{CatchupWithPolling: true}, 2},
		{"force polling", &SubscriberConfig{ForcePolling: true}, 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sub, stop := startLivenessSub(t, c.cfg, 120, 100)
			defer stop()

			deadline := time.Now().Add(3 * time.Second)
			for time.Now().Before(deadline) {
				live, total, complete := sub.TransportStatus()
				if live > total {
					t.Fatalf("live=%d > total=%d: a stream went live before it was counted", live, total)
				}
				if complete {
					if total != c.wantTotal {
						t.Fatalf("complete with total=%d, want %d streams", total, c.wantTotal)
					}
					return
				}
				time.Sleep(2 * time.Millisecond)
			}
			live, total, _ := sub.TransportStatus()
			t.Fatalf("never reported caught up: live=%d total=%d (want %d/%d)", live, total, c.wantTotal, c.wantTotal)
		})
	}
}

// TestLivenessPollerNotLiveWhileBehind: a poller holds no session, so "live"
// is defined as following the tip. One far behind it is still backfilling and
// must not read as caught up — the case the tip condition exists for.
func TestLivenessPollerNotLiveWhileBehind(t *testing.T) {
	// Tip ten million blocks ahead: far more than can be polled in the window.
	sub, stop := startLivenessSub(t, &SubscriberConfig{ForcePolling: true}, 10_000_000, 0)
	defer stop()

	sawCounted := false
	for end := time.Now().Add(300 * time.Millisecond); time.Now().Before(end); time.Sleep(5 * time.Millisecond) {
		live, total, complete := sub.TransportStatus()
		if complete || live > 0 {
			t.Fatalf("live=%d total=%d complete=%v while every poller is millions of blocks behind", live, total, complete)
		}
		if total == 2 {
			sawCounted = true
		}
	}
	if !sawCounted {
		t.Error("the backfilling pollers were never counted in total")
	}
}

// syncBuffer is a bytes.Buffer safe to write from the subscriber's goroutines.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

// TestTransportStatusLoggedOnEveryTransport: the periodic "transport status"
// line used to start only on firehose-keys, so a rollback via IBIS_TRANSPORT
// silently lost it.
func TestTransportStatusLoggedOnEveryTransport(t *testing.T) {
	cases := []struct {
		name string
		cfg  *SubscriberConfig
	}{
		{"firehose-keys", &SubscriberConfig{KeysFirehose: true, OptionSelectors: []*felt.Felt{newTestFelt(0x999)}}},
		{"shared firehose", &SubscriberConfig{SharedFirehose: true}},
		{"per-contract", &SubscriberConfig{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			handlers := map[string]func(json.RawMessage) (interface{}, error){
				"starknet_blockNumber": func(_ json.RawMessage) (interface{}, error) { return uint64(120), nil },
				"starknet_getEvents": func(_ json.RawMessage) (interface{}, error) {
					return map[string]interface{}{"events": []interface{}{}}, nil
				},
			}
			server := mockRPCServer(t, handlers)
			defer server.Close()
			var logs syncBuffer
			p, err := New(context.Background(), server.URL, slog.New(slog.NewTextHandler(&logs, nil)))
			if err != nil {
				t.Fatalf("New() error: %v", err)
			}
			defer p.Close()
			sub := p.NewSubscriber(
				[]ContractSubscription{{Address: newTestFelt(0xA), StartBlock: 100, Wildcard: true}},
				make(chan RawEvent, 64), c.cfg)
			sub.dialWSS = mockWSSDialerKeyed(nil, nil)
			sub.statusInterval = 10 * time.Millisecond

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			go sub.Start(ctx)

			for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
				if strings.Contains(logs.String(), "transport status") {
					return
				}
			}
			t.Fatal(`no "transport status" log line on this transport`)
		})
	}
}
