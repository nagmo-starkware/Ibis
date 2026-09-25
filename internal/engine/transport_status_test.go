package engine

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/NethermindEth/juno/core/felt"

	"github.com/b-j-roberts/ibis/internal/provider"
)

// backfillRPC answers the JSON-RPC methods a dynamic backfill needs, with the
// tip at 1000 and getEvents deferring to the caller.
func backfillRPC(getEvents func() interface{}) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		var result interface{}
		switch req.Method {
		case "starknet_specVersion":
			result = "0.9.0"
		case "starknet_blockNumber":
			result = 1000
		case "starknet_getEvents":
			result = getEvents()
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"jsonrpc": "2.0", "id": req.ID, "result": result})
	}))
}

// TestEngineProxiesTransportStatus: /v1/catchup_status reads these proxies, so
// a wrong delegation would let the promote gate pass mid-backfill while every
// provider-level test stays green.
func TestEngineProxiesTransportStatus(t *testing.T) {
	var e Engine
	if live, total, complete := e.TransportStatus(); live != 0 || total != 0 || complete {
		t.Fatalf("no subscriber: got live=%d total=%d complete=%v, want 0/0/false", live, total, complete)
	}
	if got := e.BackfillsPending(); got != 0 {
		t.Fatalf("no subscriber: BackfillsPending = %d, want 0", got)
	}

	release := make(chan struct{})
	srv := backfillRPC(func() interface{} {
		<-release // hold the backfill in flight
		return map[string]interface{}{"events": []interface{}{}}
	})
	defer srv.Close()
	// Runs before srv.Close, which would otherwise wait on the held handler.
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()

	ctx := context.Background()
	p, err := provider.New(ctx, srv.URL, nil)
	if err != nil {
		t.Fatalf("provider.New: %v", err)
	}
	defer p.Close()
	e.subscriber = p.NewSubscriber(nil, make(chan provider.RawEvent, 8), &provider.SubscriberConfig{SharedFirehose: true})
	e.subscriber.AddContract(ctx, provider.ContractSubscription{Address: new(felt.Felt).SetUint64(0xC0FFEE), StartBlock: 900})

	if got := e.BackfillsPending(); got != 1 {
		t.Fatalf("BackfillsPending = %d, want 1 while the backfill is in flight", got)
	}
	live, total, complete := e.TransportStatus()
	wantLive, wantTotal, wantComplete := e.subscriber.TransportStatus()
	if live != wantLive || total != wantTotal || complete != wantComplete {
		t.Fatalf("TransportStatus = %d/%d/%v, subscriber says %d/%d/%v",
			live, total, complete, wantLive, wantTotal, wantComplete)
	}

	unblock()
	for deadline := time.Now().Add(3 * time.Second); e.BackfillsPending() != 0; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("BackfillsPending = %d after the backfill finished, want 0", e.BackfillsPending())
		}
	}
}
