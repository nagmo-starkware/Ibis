package engine

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/NethermindEth/juno/core/felt"

	"github.com/b-j-roberts/ibis/internal/provider"
	"github.com/b-j-roberts/ibis/internal/store/memory"
	"github.com/b-j-roberts/ibis/internal/types"
)

// TestEngine_FreezeContract_KeepsBackfill: a child frozen while its backfill is
// in flight (e.g. registered already past its expiry predicate) must still get
// its history. Covers every transport.
func TestEngine_FreezeContract_KeepsBackfill(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  *provider.SubscriberConfig
	}{
		{"shared firehose", &provider.SubscriberConfig{SharedFirehose: true}},
		{"keys firehose", &provider.SubscriberConfig{KeysFirehose: true, OptionSelectors: []*felt.Felt{new(felt.Felt).SetUint64(0x999)}}},
		{"per-contract", &provider.SubscriberConfig{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			addr := new(felt.Felt).SetUint64(0xC0FFEE)

			release := make(chan struct{})
			srv := backfillRPC(func() interface{} {
				<-release // hold the backfill in flight until the freeze lands
				return map[string]interface{}{"events": []map[string]interface{}{{
					"block_number": 950, "block_hash": "0x1", "transaction_hash": "0x2",
					"from_address": addr.String(), "keys": []string{"0x4"}, "data": []string{"0x5"},
				}}}
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

			events := make(chan provider.RawEvent, 8)
			cs := testContractState(addr, "Child_c0ffee", nil, types.TableTypeLog)
			cs.config.Dynamic = true
			e := &Engine{store: memory.New(), logger: noopLogger(), contracts: []*contractState{cs}}
			e.subscriber = p.NewSubscriber(nil, events, tc.cfg)
			e.subscriber.AddContract(ctx, provider.ContractSubscription{Address: addr, StartBlock: 900})

			if err := e.FreezeContract(ctx, "Child_c0ffee"); err != nil {
				t.Fatalf("FreezeContract: %v", err)
			}
			if !cs.config.Frozen {
				t.Fatal("contract not marked Frozen")
			}
			unblock()

			select {
			case <-events:
			case <-time.After(10 * time.Second):
				t.Fatal("frozen contract's backfill delivered no events")
			}
			deadline := time.Now().Add(10 * time.Second)
			for e.BackfillsPending() != 0 {
				if time.Now().After(deadline) {
					t.Fatalf("BackfillsPending = %d after the freeze, want 0", e.BackfillsPending())
				}
				time.Sleep(5 * time.Millisecond)
			}
		})
	}
}
