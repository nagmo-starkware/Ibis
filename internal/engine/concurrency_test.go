package engine

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NethermindEth/juno/core/felt"
	"github.com/NethermindEth/starknet.go/rpc"

	"github.com/b-j-roberts/ibis/internal/abi"
	"github.com/b-j-roberts/ibis/internal/config"
	"github.com/b-j-roberts/ibis/internal/store/memory"
	"github.com/b-j-roberts/ibis/internal/types"
)

// concurrencyProbeProvider records the maximum number of Call()s in flight at once.
type concurrencyProbeProvider struct {
	cur    atomic.Int64
	maxObs atomic.Int64
	result []*felt.Felt
}

func (p *concurrencyProbeProvider) BlockNumber(_ context.Context) (uint64, error) { return 1, nil }

func (p *concurrencyProbeProvider) Call(_ context.Context, _, _ *felt.Felt, _ []*felt.Felt, _ rpc.BlockID) ([]*felt.Felt, error) {
	n := p.cur.Add(1)
	for {
		m := p.maxObs.Load()
		if n <= m || p.maxObs.CompareAndSwap(m, n) {
			break
		}
	}
	time.Sleep(15 * time.Millisecond) // hold the slot so concurrency can build
	p.cur.Add(-1)
	return p.result, nil
}

// Many views booting at once must not exceed the global concurrency cap — this
// is what prevents the startup wave from tripping the RPC provider's 429.
func TestViewPoller_BoundsStartupConcurrency(t *testing.T) {
	st := memory.New()
	prov := &concurrencyProbeProvider{result: []*felt.Felt{new(felt.Felt).SetUint64(1)}}
	vp := NewViewPoller(prov, st, noopLogger())

	funcDef := testFunctionDef("v")
	const n = 200
	css := make([]*contractState, 0, n)
	for i := 0; i < n; i++ {
		addr := new(felt.Felt).SetUint64(uint64(0x1000 + i))
		// Constant views poll immediately (no jitter), so all n fire their
		// initial read at once — maximally stressing the semaphore.
		css = append(css, &contractState{
			config: config.ContractConfig{
				Name:    fmt.Sprintf("C%d", i),
				Address: addr.String(),
				Views: []config.ViewConfig{{
					Function: "v",
					Refresh:  &config.ViewRefreshConfig{Mode: config.RefreshModeConstant},
					Table:    config.TableConfig{Type: "unique", UniqueKey: "_view_key"},
				}},
			},
			address: addr,
			abi:     &abi.ABI{Types: map[string]*abi.TypeDef{}, Functions: map[string]*abi.FunctionDef{"v": funcDef}},
			schemas: map[string]*types.TableSchema{},
		})
	}

	schemas, err := vp.Setup(css)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range schemas {
		if err := st.CreateTable(context.Background(), s); err != nil {
			t.Fatal(err)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	vp.Run(ctx) // constant views poll once each then exit, so Run returns

	maxObs := prov.maxObs.Load()
	if maxObs == 0 {
		t.Fatal("no polls ran")
	}
	if maxObs > maxConcurrentPolls {
		t.Fatalf("max concurrent polls %d exceeded cap %d", maxObs, maxConcurrentPolls)
	}
}
