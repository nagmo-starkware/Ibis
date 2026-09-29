package engine

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/NethermindEth/juno/core/felt"

	"github.com/b-j-roberts/ibis/internal/abi"
	"github.com/b-j-roberts/ibis/internal/config"
	"github.com/b-j-roberts/ibis/internal/provider"
	"github.com/b-j-roberts/ibis/internal/store/memory"
	"github.com/b-j-roberts/ibis/internal/types"
)

// eventsRPC is backfillRPC (tip 1000) that also records each getEvents
// request's raw params and answers with one event at block 950.
type eventsRPC struct {
	*httptest.Server
	mu     sync.Mutex
	params []string
}

func (s *eventsRPC) requests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.params...)
}

func newEventsRPC(addr *felt.Felt, sel *felt.Felt) *eventsRPC {
	s := &eventsRPC{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		var result interface{}
		switch req.Method {
		case "starknet_specVersion":
			result = "0.9.0"
		case "starknet_blockNumber":
			result = 1000
		case "starknet_getBlockWithTxHashes":
			result = map[string]interface{}{"timestamp": 1}
		case "starknet_getEvents":
			s.mu.Lock()
			s.params = append(s.params, string(req.Params))
			s.mu.Unlock()
			result = map[string]interface{}{"events": []map[string]interface{}{{
				"block_number": 950, "block_hash": "0x1", "transaction_hash": "0x2",
				"from_address": addr.String(), "keys": []string{sel.String(), "0x7"}, "data": []string{"0x5"},
			}}}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"jsonrpc": "2.0", "id": req.ID, "result": result})
	}))
	return s
}

// resumeHarness builds an engine holding one frozen dynamic contract, as
// setup() would rehydrate it, with `saved` as its persisted record.
func resumeHarness(t *testing.T, saved config.ContractConfig, cursor uint64, transport string) (*Engine, *contractState, *eventsRPC) {
	t.Helper()
	addr := new(felt.Felt).SetUint64(0xC0FFEE)
	ev := testEventDef("Transfer")
	cs := testContractState(addr, "Child_c0ffee", []*abi.EventDef{ev}, types.TableTypeLog)
	saved.Name, saved.Address, saved.Dynamic = "Child_c0ffee", addr.String(), true
	cs.config = saved

	st := memory.New()
	ctx := context.Background()
	if err := st.CreateTable(ctx, cs.schemas["Transfer"]); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveDynamicContract(ctx, &saved); err != nil {
		t.Fatal(err)
	}
	if cursor > 0 {
		if err := st.SetCursor(ctx, cs.config.Name, cursor); err != nil {
			t.Fatal(err)
		}
	}

	srv := newEventsRPC(addr, ev.Selector)
	t.Cleanup(srv.Close)
	p, err := provider.New(ctx, srv.URL, nil)
	if err != nil {
		t.Fatalf("provider.New: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })

	cfg := &config.Config{}
	cfg.Indexer.Transport = transport
	e := New(cfg, st, p, noopLogger())
	e.contracts = []*contractState{cs}
	e.setupDone = true
	return e, cs, srv
}

// runEngine runs e until the test ends and returns a channel of delivered
// events' block numbers.
func runEngine(t *testing.T, e *Engine) <-chan uint64 {
	t.Helper()
	delivered := make(chan uint64, 16)
	e.onEvent = func(_, _, _ string, block, _ uint64, _ map[string]any) { delivered <- block }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = e.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	return delivered
}

func waitPendingZero(t *testing.T, e *Engine) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for e.BackfillsPending() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("BackfillsPending = %d, want 0", e.BackfillsPending())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func persistedBackfillTo(t *testing.T, e *Engine) uint64 {
	t.Helper()
	got, err := e.store.GetDynamicContracts(context.Background())
	if err != nil || len(got) != 1 {
		t.Fatalf("GetDynamicContracts: %v (%d)", err, len(got))
	}
	return got[0].BackfillTo
}

// A frozen contract whose backfill was cut short by a restart gets a bounded
// backfill-only fetch of the missing range, from its cursor, on every transport.
func TestRun_ResumesFrozenBackfill(t *testing.T) {
	for _, transport := range []string{"http", "firehose", "firehose-keys"} {
		t.Run(transport, func(t *testing.T) {
			start := uint64(900)
			e, cs, srv := resumeHarness(t,
				config.ContractConfig{Frozen: true, StartBlock: &start, BackfillTo: 990}, 940, transport)
			e.optionSelectors = []*felt.Felt{new(felt.Felt).SetUint64(0x999)} // keys firehose needs a non-empty set
			delivered := runEngine(t, e)

			select {
			case b := <-delivered:
				if b != 950 {
					t.Fatalf("delivered block %d, want 950", b)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("no event delivered from the resumed backfill")
			}
			waitPendingZero(t, e)

			reqs := srv.requests()
			if len(reqs) != 1 {
				t.Fatalf("getEvents calls = %d, want 1: %v", len(reqs), reqs)
			}
			if !strings.Contains(reqs[0], `"block_number":940`) || !strings.Contains(reqs[0], `"block_number":990`) {
				t.Fatalf("getEvents range not [940, 990] (cursor..BackfillTo): %s", reqs[0])
			}
			// The firehose transports run their one shared stream regardless of
			// contracts, so only the per-contract transport can show it.
			if _, total, _ := e.TransportStatus(); transport == "http" && total != 0 {
				t.Fatalf("live streams = %d, want 0 (frozen: backfill only)", total)
			}
			if !cs.config.Frozen {
				t.Fatal("contract no longer frozen")
			}
			// Marker cleared once the backfill completes, in memory and store.
			deadline := time.Now().Add(5 * time.Second)
			for persistedBackfillTo(t, e) != 0 {
				if time.Now().After(deadline) {
					t.Fatal("BackfillTo still persisted after the backfill completed")
				}
				time.Sleep(5 * time.Millisecond)
			}
		})
	}
}

// Complete history (BackfillTo == 0) is never re-fetched. The legacy case is a
// record persisted before the field existed: it must read as complete, or
// every restart would rescan all frozen children.
func TestRun_DoesNotRefetchCompleteFrozenContract(t *testing.T) {
	var legacy config.ContractConfig
	if err := json.Unmarshal([]byte(`{"name":"x","frozen":true,"start_block":900}`), &legacy); err != nil {
		t.Fatal(err)
	}
	start := uint64(900)
	for name, saved := range map[string]config.ContractConfig{
		"complete": {Frozen: true, StartBlock: &start},
		"legacy":   legacy,
	} {
		t.Run(name, func(t *testing.T) {
			e, _, srv := resumeHarness(t, saved, 0, "http")
			runEngine(t, e)
			time.Sleep(300 * time.Millisecond)
			if reqs := srv.requests(); len(reqs) != 0 {
				t.Fatalf("getEvents calls = %v, want none", reqs)
			}
			if got := e.BackfillsPending(); got != 0 {
				t.Fatalf("BackfillsPending = %d, want 0", got)
			}
		})
	}
}

// A cursor already past BackfillTo leaves nothing to fetch; the marker clears.
func TestResumeFrozenBackfills_NothingLeft(t *testing.T) {
	start := uint64(900)
	e, cs, srv := resumeHarness(t,
		config.ContractConfig{Frozen: true, StartBlock: &start, BackfillTo: 990}, 995, "http")
	e.subscriber = e.provider.NewSubscriber(nil, e.events, &provider.SubscriberConfig{OnBackfillDone: e.onBackfillDone})
	e.resumeFrozenBackfills(context.Background())

	if reqs := srv.requests(); len(reqs) != 0 {
		t.Fatalf("getEvents calls = %v, want none", reqs)
	}
	if cs.config.BackfillTo != 0 || persistedBackfillTo(t, e) != 0 {
		t.Fatal("BackfillTo not cleared")
	}
}

// Freezing with a backfill in flight persists where it ends; completion clears it.
func TestEngine_FreezeContract_BackfillMarker(t *testing.T) {
	for name, cfg := range map[string]*provider.SubscriberConfig{
		"shared firehose": {SharedFirehose: true},
		"per-contract":    {},
	} {
		t.Run(name, func(t *testing.T) {
			addr := new(felt.Felt).SetUint64(0xC0FFEE)
			release := make(chan struct{})
			srv := backfillRPC(func() interface{} {
				<-release
				return map[string]interface{}{"events": []interface{}{}}
			})
			defer srv.Close()
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			defer unblock()

			ctx := context.Background()
			p, err := provider.New(ctx, srv.URL, nil)
			if err != nil {
				t.Fatalf("provider.New: %v", err)
			}
			defer p.Close()

			cs := testContractState(addr, "Child_c0ffee", nil, types.TableTypeLog)
			cs.config.Dynamic = true
			e := &Engine{store: memory.New(), logger: noopLogger(), contracts: []*contractState{cs}}
			cfg.OnBackfillDone = e.onBackfillDone
			e.subscriber = p.NewSubscriber(nil, make(chan provider.RawEvent, 8),
				cfg)
			e.subscriber.AddContract(ctx, provider.ContractSubscription{Address: addr, StartBlock: 900})

			if err := e.FreezeContract(ctx, "Child_c0ffee"); err != nil {
				t.Fatalf("FreezeContract: %v", err)
			}
			if got := persistedBackfillTo(t, e); got != 1000 {
				t.Fatalf("persisted BackfillTo = %d mid-backfill, want the tip 1000", got)
			}

			unblock()
			deadline := time.Now().Add(10 * time.Second)
			for persistedBackfillTo(t, e) != 0 {
				if time.Now().After(deadline) {
					t.Fatal("marker not cleared after the backfill completed")
				}
				time.Sleep(5 * time.Millisecond)
			}
			waitPendingZero(t, e)
		})
	}
}

// A contract frozen with no backfill in flight has nothing to resume.
func TestEngine_FreezeContract_NoBackfillNoMarker(t *testing.T) {
	addr := new(felt.Felt).SetUint64(0xC0FFEE)
	cs := testContractState(addr, "Child_c0ffee", nil, types.TableTypeLog)
	cs.config.Dynamic = true
	e := &Engine{store: memory.New(), logger: noopLogger(), contracts: []*contractState{cs}}
	if err := e.FreezeContract(context.Background(), "Child_c0ffee"); err != nil {
		t.Fatal(err)
	}
	if got := persistedBackfillTo(t, e); got != 0 {
		t.Fatalf("BackfillTo = %d, want 0", got)
	}
}
