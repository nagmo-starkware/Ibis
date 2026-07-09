package provider

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var errBlockNumberUnavailable = errors.New("block number unavailable")

// blockNumberCounter returns a starknet_blockNumber handler that returns `value`
// and increments `calls` on every invocation. Use to assert how many real RPC
// tip reads happen.
func blockNumberCounter(calls *atomic.Int64, value func() uint64) map[string]func(json.RawMessage) (interface{}, error) {
	return map[string]func(json.RawMessage) (interface{}, error){
		"starknet_blockNumber": func(_ json.RawMessage) (interface{}, error) {
			calls.Add(1)
			return value(), nil
		},
	}
}

// TestCachedBlockNumberColdFetch: with an unprimed cache, the first call fetches
// directly (1 RPC) and subsequent calls are served from the cache (0 further RPC).
func TestCachedBlockNumberColdFetch(t *testing.T) {
	var calls atomic.Int64
	server := mockRPCServer(t, blockNumberCounter(&calls, func() uint64 { return 4242 }))
	defer server.Close()

	p, err := New(context.Background(), server.URL, nil)
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	defer p.Close()

	got, err := p.CachedBlockNumber(context.Background())
	if err != nil {
		t.Fatalf("CachedBlockNumber() error: %v", err)
	}
	if got != 4242 {
		t.Errorf("CachedBlockNumber() = %d, want 4242", got)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("cold fetch made %d RPC calls, want 1", n)
	}

	// Second call within the freshness window: cache hit, no extra RPC.
	if _, err := p.CachedBlockNumber(context.Background()); err != nil {
		t.Fatalf("CachedBlockNumber() 2nd error: %v", err)
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("second call added RPC calls: got %d, want 1", n)
	}
}

// TestCachedBlockNumberServesCache: a primed, fresh cache serves many concurrent
// readers with zero RPC calls — the core O(N contracts) -> O(1) reduction.
func TestCachedBlockNumberServesCache(t *testing.T) {
	var calls atomic.Int64
	server := mockRPCServer(t, blockNumberCounter(&calls, func() uint64 { return 500 }))
	defer server.Close()

	p, err := New(context.Background(), server.URL, nil)
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	defer p.Close()

	// Prime a fresh cache directly (white-box).
	p.tipBlock.Store(500)
	p.tipUpdated.Store(time.Now().UnixNano())
	p.tipIntervalNanos.Store(int64(2 * time.Second))

	const readers, reads = 50, 20
	var wg sync.WaitGroup
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < reads; j++ {
				bn, err := p.CachedBlockNumber(context.Background())
				if err != nil || bn != 500 {
					t.Errorf("CachedBlockNumber() = %d, %v; want 500, nil", bn, err)
				}
			}
		}()
	}
	wg.Wait()

	if n := calls.Load(); n != 0 {
		t.Fatalf("%d cached reads made %d RPC calls, want 0", readers*reads, n)
	}
}

// TestCachedBlockNumberStaleRefetch: once the cache is older than 4x the poll
// interval (a wedged/stopped poller), the next call refetches.
func TestCachedBlockNumberStaleRefetch(t *testing.T) {
	var calls atomic.Int64
	server := mockRPCServer(t, blockNumberCounter(&calls, func() uint64 { return 999 }))
	defer server.Close()

	p, err := New(context.Background(), server.URL, nil)
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	defer p.Close()

	// Stale by construction: updated 10s ago, interval 1s -> maxStale 4s.
	p.tipBlock.Store(500)
	p.tipUpdated.Store(time.Now().Add(-10 * time.Second).UnixNano())
	p.tipIntervalNanos.Store(int64(time.Second))

	got, err := p.CachedBlockNumber(context.Background())
	if err != nil {
		t.Fatalf("CachedBlockNumber() error: %v", err)
	}
	if got != 999 {
		t.Errorf("stale cache returned %d, want refetched 999", got)
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("stale refetch made %d RPC calls, want 1", n)
	}
}

// TestCachedBlockNumberStaleFetchFailsReturnsStale: when the fallback fetch
// fails but a prior value exists, the stale value is returned rather than an
// error (a slightly old tip only delays delivery; an error aborts the caller).
func TestCachedBlockNumberStaleFetchFailsReturnsStale(t *testing.T) {
	server := mockRPCServer(t, map[string]func(json.RawMessage) (interface{}, error){
		"starknet_blockNumber": func(_ json.RawMessage) (interface{}, error) {
			return nil, errBlockNumberUnavailable
		},
	})
	defer server.Close()

	p, err := New(context.Background(), server.URL, nil)
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	defer p.Close()

	p.tipBlock.Store(500)
	p.tipUpdated.Store(time.Now().Add(-10 * time.Second).UnixNano())
	p.tipIntervalNanos.Store(int64(time.Second))

	got, err := p.CachedBlockNumber(context.Background())
	if err != nil {
		t.Fatalf("CachedBlockNumber() should return stale value, got error: %v", err)
	}
	if got != 500 {
		t.Errorf("got %d, want stale 500", got)
	}
}

// TestStartTipPollerSharesOneCall is the headline test: a single background
// poller serves 1000 concurrent reads with ~1 RPC call (the prime), instead of
// one call per read.
func TestStartTipPollerSharesOneCall(t *testing.T) {
	var calls atomic.Int64
	server := mockRPCServer(t, blockNumberCounter(&calls, func() uint64 { return 1000 }))
	defer server.Close()

	p, err := New(context.Background(), server.URL, nil)
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	defer p.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Long interval so no background tick fires during the read burst.
	p.StartTipPoller(ctx, 500*time.Millisecond)

	const readers, reads = 50, 20
	var wg sync.WaitGroup
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < reads; j++ {
				if bn, err := p.CachedBlockNumber(ctx); err != nil || bn != 1000 {
					t.Errorf("CachedBlockNumber() = %d, %v; want 1000, nil", bn, err)
				}
			}
		}()
	}
	wg.Wait()
	cancel()

	// Prime is 1 call; allow a small slack for a stray tick. The point is it is
	// O(1), not O(readers*reads)=1000.
	if n := calls.Load(); n < 1 || n > 3 {
		t.Fatalf("%d reads triggered %d blockNumber RPC calls, want ~1 (O(1), not O(N))", readers*reads, n)
	}
}

// TestStartTipPollerRefreshes: the background poller picks up chain advancement.
func TestStartTipPollerRefreshes(t *testing.T) {
	var tip atomic.Uint64
	tip.Store(100)
	server := mockRPCServer(t, map[string]func(json.RawMessage) (interface{}, error){
		"starknet_blockNumber": func(_ json.RawMessage) (interface{}, error) {
			return tip.Load(), nil
		},
	})
	defer server.Close()

	p, err := New(context.Background(), server.URL, nil)
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	defer p.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	p.StartTipPoller(ctx, 10*time.Millisecond)

	// Primed at 100.
	if bn, _ := p.CachedBlockNumber(ctx); bn != 100 {
		t.Fatalf("primed tip = %d, want 100", bn)
	}

	// Advance the chain; the background poller should reflect it soon.
	tip.Store(200)
	deadline := time.Now().Add(2 * time.Second)
	for {
		bn, _ := p.CachedBlockNumber(ctx)
		if bn == 200 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("tip poller did not refresh to 200 within deadline (last=%d)", bn)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestNewSubscriberResolvesPollingConfig: SubscriberConfig overrides apply, and
// omitted values fall back to the package defaults.
func TestNewSubscriberResolvesPollingConfig(t *testing.T) {
	server := mockRPCServer(t, blockNumberCounter(new(atomic.Int64), func() uint64 { return 1 }))
	defer server.Close()
	p, err := New(context.Background(), server.URL, nil)
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	defer p.Close()

	events := make(chan RawEvent, 1)

	// Overrides applied.
	s := p.NewSubscriber(nil, events, &SubscriberConfig{
		TipPollInterval:     7 * time.Second,
		CatchupPollInterval: 250 * time.Millisecond,
		MaxConcurrentPolls:  4,
	})
	if s.tipPollInterval != 7*time.Second {
		t.Errorf("tipPollInterval = %v, want 7s", s.tipPollInterval)
	}
	if s.catchupPollInterval != 250*time.Millisecond {
		t.Errorf("catchupPollInterval = %v, want 250ms", s.catchupPollInterval)
	}
	if cap(s.sem) != 4 {
		t.Errorf("sem cap = %d, want 4", cap(s.sem))
	}

	// Defaults when unset.
	d := p.NewSubscriber(nil, events, &SubscriberConfig{})
	if d.tipPollInterval != defaultTipPollInterval {
		t.Errorf("default tipPollInterval = %v, want %v", d.tipPollInterval, defaultTipPollInterval)
	}
	if d.catchupPollInterval != defaultCatchupPollInterval {
		t.Errorf("default catchupPollInterval = %v, want %v", d.catchupPollInterval, defaultCatchupPollInterval)
	}
	if cap(d.sem) != maxConcurrentCatchup {
		t.Errorf("default sem cap = %d, want %d", cap(d.sem), maxConcurrentCatchup)
	}

	// Nil config: defaults too.
	n := p.NewSubscriber(nil, events, nil)
	if n.tipPollInterval != defaultTipPollInterval || cap(n.sem) != maxConcurrentCatchup {
		t.Errorf("nil config did not apply defaults: interval=%v sem=%d", n.tipPollInterval, cap(n.sem))
	}
}

// TestSubscriberTipBlockNumberGate: with sharedTipPoller off, each tip read hits
// RPC directly (legacy); on, reads are served from the shared cache; and the
// firehose transport implies the poller.
func TestSubscriberTipBlockNumberGate(t *testing.T) {
	var calls atomic.Int64
	server := mockRPCServer(t, blockNumberCounter(&calls, func() uint64 { return 900 }))
	defer server.Close()
	p, err := New(context.Background(), server.URL, nil)
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	defer p.Close()
	events := make(chan RawEvent, 1)

	// Off (default): each call is a direct, uncached BlockNumber.
	off := p.NewSubscriber(nil, events, &SubscriberConfig{})
	if off.sharedTipPoller {
		t.Fatal("sharedTipPoller should default to false")
	}
	for i := 0; i < 3; i++ {
		if bn, err := off.tipBlockNumber(context.Background()); err != nil || bn != 900 {
			t.Fatalf("tipBlockNumber = %d, %v; want 900, nil", bn, err)
		}
	}
	if n := calls.Load(); n != 3 {
		t.Errorf("off: %d RPC calls, want 3 (uncached)", n)
	}

	// On: a primed cache serves reads with no further RPC.
	calls.Store(0)
	p.tipBlock.Store(900)
	p.tipUpdated.Store(time.Now().UnixNano())
	p.tipIntervalNanos.Store(int64(2 * time.Second))
	on := p.NewSubscriber(nil, events, &SubscriberConfig{SharedTipPoller: true})
	if !on.sharedTipPoller {
		t.Fatal("sharedTipPoller should be true")
	}
	for i := 0; i < 3; i++ {
		if bn, _ := on.tipBlockNumber(context.Background()); bn != 900 {
			t.Fatalf("cached tip = %d, want 900", bn)
		}
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("on: %d RPC calls, want 0 (served from cache)", n)
	}

	// The firehose transport implies the shared tip poller.
	if fh := p.NewSubscriber(nil, events, &SubscriberConfig{SharedFirehose: true}); !fh.sharedTipPoller {
		t.Error("SharedFirehose should imply sharedTipPoller")
	}
}

// TestStartTipPollerStopsOnCancel: cancelling the context stops the background
// refresh (no further RPC calls after cancel settles).
func TestStartTipPollerStopsOnCancel(t *testing.T) {
	var calls atomic.Int64
	server := mockRPCServer(t, blockNumberCounter(&calls, func() uint64 { return 7 }))
	defer server.Close()

	p, err := New(context.Background(), server.URL, nil)
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	defer p.Close()

	ctx, cancel := context.WithCancel(context.Background())
	p.StartTipPoller(ctx, 10*time.Millisecond)
	time.Sleep(35 * time.Millisecond) // let a few ticks fire
	cancel()
	time.Sleep(30 * time.Millisecond) // let the goroutine observe cancel
	settled := calls.Load()
	time.Sleep(40 * time.Millisecond) // window in which no new calls should occur
	if n := calls.Load(); n != settled {
		t.Errorf("tip poller kept polling after cancel: %d -> %d", settled, n)
	}
}
