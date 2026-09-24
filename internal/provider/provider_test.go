package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NethermindEth/juno/core/felt"
	"github.com/NethermindEth/starknet.go/client"
	"github.com/NethermindEth/starknet.go/rpc"
)

// --- URL Conversion Tests ---

func TestToHTTPURL(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"wss://rpc.example.com/v3/key", "https://rpc.example.com/v3/key"},
		{"ws://localhost:5050", "http://localhost:5050"},
		{"https://rpc.example.com/v3/key", "https://rpc.example.com/v3/key"},
		{"http://localhost:5050", "http://localhost:5050"},
		{"rpc.example.com", "rpc.example.com"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := ToHTTPURL(tt.input)
			if got != tt.want {
				t.Errorf("ToHTTPURL(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestToWSURL(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"https://rpc.example.com/v3/key", "wss://rpc.example.com/v3/key"},
		{"http://localhost:5050", "ws://localhost:5050"},
		{"wss://rpc.example.com/v3/key", "wss://rpc.example.com/v3/key"},
		{"ws://localhost:5050", "ws://localhost:5050"},
		{"rpc.example.com", "rpc.example.com"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := ToWSURL(tt.input)
			if got != tt.want {
				t.Errorf("ToWSURL(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestURLConversionRoundTrip(t *testing.T) {
	urls := []string{
		"https://starknet-sepolia.infura.io/v3/key123",
		"http://localhost:5050",
	}

	for _, url := range urls {
		wsURL := ToWSURL(url)
		httpURL := ToHTTPURL(wsURL)
		if httpURL != url {
			t.Errorf("round-trip failed: %q → %q → %q", url, wsURL, httpURL)
		}
	}
}

// --- JSON-RPC Mock Server ---

type jsonRPCRequest struct {
	ID      int             `json:"id"`
	JSONRPC string          `json:"jsonrpc"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type jsonRPCResponse struct {
	ID      int         `json:"id"`
	JSONRPC string      `json:"jsonrpc"`
	Result  interface{} `json:"result,omitempty"`
	Error   interface{} `json:"error,omitempty"`
}

// mockRPCServer creates an HTTP test server that responds to JSON-RPC calls.
// Automatically handles starknet_specVersion (required by starknet.go's NewProvider).
// The handler map keys are method names, values are result generators.
func mockRPCServer(t *testing.T, handlers map[string]func(params json.RawMessage) (interface{}, error)) *httptest.Server {
	t.Helper()

	// Always include specVersion handler (required for provider init).
	if _, ok := handlers["starknet_specVersion"]; !ok {
		handlers["starknet_specVersion"] = func(_ json.RawMessage) (interface{}, error) {
			return "0.9.0", nil
		}
	}

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}

		var req jsonRPCRequest
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}

		handler, ok := handlers[req.Method]
		if !ok {
			resp := jsonRPCResponse{
				ID:      req.ID,
				JSONRPC: "2.0",
				Error:   map[string]interface{}{"code": -32601, "message": "method not found: " + req.Method},
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(resp)
			return
		}

		result, err := handler(req.Params)
		resp := jsonRPCResponse{ID: req.ID, JSONRPC: "2.0"}
		if err != nil {
			resp.Error = map[string]interface{}{"code": -32000, "message": err.Error()}
		} else {
			resp.Result = result
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
}

// --- Provider Tests ---

func TestNewProvider(t *testing.T) {
	server := mockRPCServer(t, map[string]func(json.RawMessage) (interface{}, error){})
	defer server.Close()

	p, err := New(context.Background(), server.URL, nil)
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	defer p.Close()

	if p.HTTPURL() != server.URL {
		t.Errorf("HTTPURL() = %q, want %q", p.HTTPURL(), server.URL)
	}

	wantWS := ToWSURL(server.URL)
	if p.WSURL() != wantWS {
		t.Errorf("WSURL() = %q, want %q", p.WSURL(), wantWS)
	}
}

func TestProviderBlockNumber(t *testing.T) {
	server := mockRPCServer(t, map[string]func(json.RawMessage) (interface{}, error){
		"starknet_blockNumber": func(_ json.RawMessage) (interface{}, error) {
			return 42000, nil
		},
	})
	defer server.Close()

	p, err := New(context.Background(), server.URL, nil)
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	blockNum, err := p.BlockNumber(context.Background())
	if err != nil {
		t.Fatalf("BlockNumber() error: %v", err)
	}
	if blockNum != 42000 {
		t.Errorf("BlockNumber() = %d, want 42000", blockNum)
	}
}

func TestProviderGetEvents(t *testing.T) {
	callCount := 0
	server := mockRPCServer(t, map[string]func(json.RawMessage) (interface{}, error){
		"starknet_getEvents": func(params json.RawMessage) (interface{}, error) {
			callCount++

			// First call returns events + continuation token.
			// Second call returns remaining events.
			if callCount == 1 {
				return map[string]interface{}{
					"events": []map[string]interface{}{
						{
							"block_number":     100,
							"block_hash":       "0x1",
							"transaction_hash": "0x2",
							"from_address":     "0x3",
							"keys":             []string{"0x4"},
							"data":             []string{"0x5"},
						},
					},
					"continuation_token": "token123",
				}, nil
			}

			return map[string]interface{}{
				"events": []map[string]interface{}{
					{
						"block_number":     101,
						"block_hash":       "0x6",
						"transaction_hash": "0x7",
						"from_address":     "0x8",
						"keys":             []string{"0x9"},
						"data":             []string{"0xa"},
					},
				},
			}, nil
		},
	})
	defer server.Close()

	p, err := New(context.Background(), server.URL, nil)
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	events, err := p.GetEvents(context.Background(), GetEventsOptions{
		FromBlock: 100,
		ToBlock:   200,
		ChunkSize: 1,
	})
	if err != nil {
		t.Fatalf("GetEvents() error: %v", err)
	}

	if len(events) != 2 {
		t.Fatalf("GetEvents() returned %d events, want 2", len(events))
	}

	if events[0].BlockNumber != 100 {
		t.Errorf("events[0].BlockNumber = %d, want 100", events[0].BlockNumber)
	}
	if events[1].BlockNumber != 101 {
		t.Errorf("events[1].BlockNumber = %d, want 101", events[1].BlockNumber)
	}

	if callCount != 2 {
		t.Errorf("expected 2 RPC calls (pagination), got %d", callCount)
	}
}

func TestProviderGetEventsContextCancelled(t *testing.T) {
	server := mockRPCServer(t, map[string]func(json.RawMessage) (interface{}, error){
		"starknet_getEvents": func(_ json.RawMessage) (interface{}, error) {
			return map[string]interface{}{
				"events":             []interface{}{},
				"continuation_token": "forever",
			}, nil
		},
	})
	defer server.Close()

	p, err := New(context.Background(), server.URL, nil)
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	_, err = p.GetEvents(ctx, GetEventsOptions{FromBlock: 0, ToBlock: 100})
	if err == nil {
		t.Fatal("expected error from canceled context")
	}
}

func TestProviderCall(t *testing.T) {
	server := mockRPCServer(t, map[string]func(json.RawMessage) (interface{}, error){
		"starknet_call": func(params json.RawMessage) (interface{}, error) {
			// Return two felt values (simulating a u256 return: low=1000, high=0).
			return []string{"0x3e8", "0x0"}, nil
		},
	})
	defer server.Close()

	p, err := New(context.Background(), server.URL, nil)
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	contractAddr := newTestFelt(0xABC)
	selector := newTestFelt(0x123)
	calldata := []*felt.Felt{newTestFelt(0x456)}

	result, err := p.Call(context.Background(), contractAddr, selector, calldata, rpc.BlockID{Tag: rpc.BlockTagLatest})
	if err != nil {
		t.Fatalf("Call() error: %v", err)
	}

	if len(result) != 2 {
		t.Fatalf("Call() returned %d felts, want 2", len(result))
	}
	if result[0].String() != newTestFelt(0x3e8).String() {
		t.Errorf("result[0] = %s, want 0x3e8", result[0].String())
	}
}

func TestProviderCallWithNilCalldata(t *testing.T) {
	server := mockRPCServer(t, map[string]func(json.RawMessage) (interface{}, error){
		"starknet_call": func(params json.RawMessage) (interface{}, error) {
			return []string{"0x42"}, nil
		},
	})
	defer server.Close()

	p, err := New(context.Background(), server.URL, nil)
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	result, err := p.Call(context.Background(), newTestFelt(0xABC), newTestFelt(0x123), nil, rpc.BlockID{Tag: rpc.BlockTagLatest})
	if err != nil {
		t.Fatalf("Call() error: %v", err)
	}
	if len(result) != 1 {
		t.Fatalf("Call() returned %d felts, want 1", len(result))
	}
}

// --- Mock WSS Session Helpers ---

// mockWSSDialerFunc returns a wssDialer that delivers the given events then errors.
func mockWSSDialerFunc(events []*rpc.EmittedEventWithFinalityStatus, sessionErr error) wssDialer {
	return func(ctx context.Context, wsURL string, input *rpc.EventSubscriptionInput) (*wssSession, error) {
		eventCh := make(chan *rpc.EmittedEventWithFinalityStatus, len(events)+1)
		errCh := make(chan error, 1)
		reorgCh := make(chan *client.ReorgEvent, 1)

		go func() {
			for _, e := range events {
				select {
				case eventCh <- e:
				case <-ctx.Done():
					return
				}
			}
			if sessionErr != nil {
				errCh <- sessionErr
			}
		}()

		return &wssSession{
			events: eventCh,
			errs:   errCh,
			reorgs: reorgCh,
			close:  func() {},
		}, nil
	}
}

// mockWSSDialerFail returns a wssDialer that always fails to connect.
func mockWSSDialerFail(err error) wssDialer {
	return func(ctx context.Context, wsURL string, input *rpc.EventSubscriptionInput) (*wssSession, error) {
		return nil, err
	}
}

// mockWSSDialerFailThenSucceed returns a wssDialer that fails N times,
// then succeeds with the given events.
func mockWSSDialerFailThenSucceed(failures int, events []*rpc.EmittedEventWithFinalityStatus) wssDialer {
	var count atomic.Int32
	return func(ctx context.Context, wsURL string, input *rpc.EventSubscriptionInput) (*wssSession, error) {
		n := count.Add(1)
		if int(n) <= failures {
			return nil, fmt.Errorf("connection refused (attempt %d)", n)
		}

		eventCh := make(chan *rpc.EmittedEventWithFinalityStatus, len(events)+1)
		errCh := make(chan error, 1)
		reorgCh := make(chan *client.ReorgEvent, 1)

		go func() {
			for _, e := range events {
				select {
				case eventCh <- e:
				case <-ctx.Done():
					return
				}
			}
			// Block until context cancellation (stay connected).
			<-ctx.Done()
		}()

		return &wssSession{
			events: eventCh,
			errs:   errCh,
			reorgs: reorgCh,
			close:  func() {},
		}, nil
	}
}

func newTestFelt(v uint64) *felt.Felt {
	return new(felt.Felt).SetUint64(v)
}

func newTestEvent(blockNum uint64) *rpc.EmittedEventWithFinalityStatus {
	return &rpc.EmittedEventWithFinalityStatus{
		EmittedEvent: rpc.EmittedEvent{
			Event: rpc.Event{
				FromAddress: newTestFelt(0xABC),
				EventContent: rpc.EventContent{
					Keys: []*felt.Felt{newTestFelt(0x1)},
					Data: []*felt.Felt{newTestFelt(0x2)},
				},
			},
			BlockNumber:     blockNum,
			BlockHash:       newTestFelt(blockNum * 10),
			TransactionHash: newTestFelt(blockNum * 100),
		},
		FinalityStatus: rpc.TxnFinalityStatusAcceptedOnL2,
	}
}

// --- Subscriber Tests ---

func TestSubscriberWSSReceivesEvents(t *testing.T) {
	server := mockRPCServer(t, map[string]func(json.RawMessage) (interface{}, error){
		"starknet_blockNumber": func(_ json.RawMessage) (interface{}, error) {
			return 1000, nil
		},
	})
	defer server.Close()

	p, err := New(context.Background(), server.URL, nil)
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	events := make(chan RawEvent, 10)
	sub := p.NewSubscriber(
		[]ContractSubscription{{Address: newTestFelt(0xABC), StartBlock: 100}},
		events,
		nil,
	)

	// Inject mock WSS dialer that delivers 3 events then signals done.
	mockEvents := []*rpc.EmittedEventWithFinalityStatus{
		newTestEvent(100),
		newTestEvent(101),
		newTestEvent(102),
	}
	sub.dialWSS = mockWSSDialerFunc(mockEvents, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	go sub.Start(ctx)

	received := make([]RawEvent, 0, 3)
	for i := 0; i < 3; i++ {
		select {
		case evt := <-events:
			received = append(received, evt)
		case <-ctx.Done():
			t.Fatalf("timed out waiting for event %d", i)
		}
	}

	if len(received) != 3 {
		t.Fatalf("received %d events, want 3", len(received))
	}

	for i, evt := range received {
		wantBlock := uint64(100 + i)
		if evt.BlockNumber != wantBlock {
			t.Errorf("event[%d].BlockNumber = %d, want %d", i, evt.BlockNumber, wantBlock)
		}
		if evt.FinalityStatus != string(rpc.TxnFinalityStatusAcceptedOnL2) {
			t.Errorf("event[%d].FinalityStatus = %q, want ACCEPTED_ON_L2", i, evt.FinalityStatus)
		}
	}
}

func TestSubscriberWSSReconnects(t *testing.T) {
	server := mockRPCServer(t, map[string]func(json.RawMessage) (interface{}, error){
		"starknet_blockNumber": func(_ json.RawMessage) (interface{}, error) {
			return 1000, nil
		},
	})
	defer server.Close()

	p, err := New(context.Background(), server.URL, nil)
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	events := make(chan RawEvent, 10)
	sub := p.NewSubscriber(
		[]ContractSubscription{{Address: newTestFelt(0xABC), StartBlock: 100}},
		events,
		nil,
	)

	// Fail 2 times, then succeed with events.
	mockEvents := []*rpc.EmittedEventWithFinalityStatus{newTestEvent(200)}
	sub.dialWSS = mockWSSDialerFailThenSucceed(2, mockEvents)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	go sub.Start(ctx)

	select {
	case evt := <-events:
		if evt.BlockNumber != 200 {
			t.Errorf("event.BlockNumber = %d, want 200", evt.BlockNumber)
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for event after reconnection")
	}
}

func TestSubscriberFallsBackToPolling(t *testing.T) {
	var pollCalls atomic.Int32

	server := mockRPCServer(t, map[string]func(json.RawMessage) (interface{}, error){
		"starknet_blockNumber": func(_ json.RawMessage) (interface{}, error) {
			return 105, nil
		},
		"starknet_getEvents": func(_ json.RawMessage) (interface{}, error) {
			n := pollCalls.Add(1)
			if n == 1 {
				return map[string]interface{}{
					"events": []map[string]interface{}{
						{
							"block_number":     100,
							"block_hash":       "0x1",
							"transaction_hash": "0x2",
							"from_address":     "0x3",
							"keys":             []string{"0x4"},
							"data":             []string{"0x5"},
						},
					},
				}, nil
			}
			return map[string]interface{}{"events": []interface{}{}}, nil
		},
	})
	defer server.Close()

	p, err := New(context.Background(), server.URL, nil)
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	events := make(chan RawEvent, 10)
	sub := p.NewSubscriber(
		[]ContractSubscription{{Address: newTestFelt(0xABC), StartBlock: 100}},
		events,
		&SubscriberConfig{BlocksPerQuery: 10},
	)

	// WSS always fails → forces polling fallback.
	sub.dialWSS = mockWSSDialerFail(fmt.Errorf("WSS not supported"))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	go sub.Start(ctx)

	select {
	case evt := <-events:
		if evt.BlockNumber != 100 {
			t.Errorf("polled event.BlockNumber = %d, want 100", evt.BlockNumber)
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for polled event")
	}
}

func TestSubscriberNoContracts(t *testing.T) {
	server := mockRPCServer(t, map[string]func(json.RawMessage) (interface{}, error){})
	defer server.Close()

	p, err := New(context.Background(), server.URL, nil)
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	events := make(chan RawEvent, 1)
	sub := p.NewSubscriber(nil, events, nil)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately so Start returns right away.

	err = sub.Start(ctx)
	if err == nil {
		t.Fatal("expected error for canceled context")
	}
}

func TestSubscriberReorgResets(t *testing.T) {
	server := mockRPCServer(t, map[string]func(json.RawMessage) (interface{}, error){
		"starknet_blockNumber": func(_ json.RawMessage) (interface{}, error) {
			return 1000, nil
		},
	})
	defer server.Close()

	p, err := New(context.Background(), server.URL, nil)
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	events := make(chan RawEvent, 10)
	sub := p.NewSubscriber(
		[]ContractSubscription{{Address: newTestFelt(0xABC), StartBlock: 100}},
		events,
		nil,
	)

	// Custom dialer that sends events, then a reorg, then more events.
	sub.dialWSS = func(ctx context.Context, wsURL string, input *rpc.EventSubscriptionInput) (*wssSession, error) {
		eventCh := make(chan *rpc.EmittedEventWithFinalityStatus, 10)
		errCh := make(chan error, 1)
		reorgCh := make(chan *client.ReorgEvent, 1)

		go func() {
			// Send initial event.
			select {
			case eventCh <- newTestEvent(150):
			case <-ctx.Done():
				return
			}

			// Send reorg event.
			select {
			case reorgCh <- &client.ReorgEvent{
				StartBlockNum: 140,
				EndBlockNum:   150,
			}:
			case <-ctx.Done():
				return
			}

			// Small delay to let reorg be processed.
			time.Sleep(50 * time.Millisecond)

			// Send post-reorg event.
			select {
			case eventCh <- newTestEvent(140):
			case <-ctx.Done():
				return
			}

			<-ctx.Done()
		}()

		return &wssSession{
			events: eventCh,
			errs:   errCh,
			reorgs: reorgCh,
			close:  func() {},
		}, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	go sub.Start(ctx)

	// Should receive: event@150, event@140 (after reorg).
	var received []uint64
	for i := 0; i < 2; i++ {
		select {
		case evt := <-events:
			received = append(received, evt.BlockNumber)
		case <-ctx.Done():
			t.Fatalf("timed out waiting for event %d, received so far: %v", i, received)
		}
	}

	if len(received) != 2 {
		t.Fatalf("received %d events, want 2", len(received))
	}
	if received[0] != 150 {
		t.Errorf("first event block = %d, want 150", received[0])
	}
	if received[1] != 140 {
		t.Errorf("second event block = %d, want 140 (post-reorg)", received[1])
	}
}

func TestSubscriberBackfill(t *testing.T) {
	callRanges := make([]struct{ from, to uint64 }, 0)

	server := mockRPCServer(t, map[string]func(json.RawMessage) (interface{}, error){
		"starknet_getEvents": func(params json.RawMessage) (interface{}, error) {
			// Track the call ranges.
			callRanges = append(callRanges, struct{ from, to uint64 }{})
			return map[string]interface{}{
				"events": []map[string]interface{}{
					{
						"block_number":     100,
						"block_hash":       "0x1",
						"transaction_hash": "0x2",
						"from_address":     "0x3",
						"keys":             []string{"0x4"},
						"data":             []string{"0x5"},
					},
				},
			}, nil
		},
	})
	defer server.Close()

	p, err := New(context.Background(), server.URL, nil)
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	events := make(chan RawEvent, 100)
	sub := p.NewSubscriber(
		[]ContractSubscription{{Address: newTestFelt(0xABC)}},
		events,
		&SubscriberConfig{BlocksPerQuery: 50},
	)

	err = sub.Backfill(context.Background(),
		ContractSubscription{Address: newTestFelt(0xABC)},
		0, 120,
	)
	if err != nil {
		t.Fatalf("Backfill() error: %v", err)
	}

	// With BlocksPerQuery=50, range [0, 120] should be split into:
	// [0, 49], [50, 99], [100, 120] = 3 calls.
	if len(callRanges) != 3 {
		t.Errorf("expected 3 backfill calls, got %d", len(callRanges))
	}
}

// mockWSSDialerSessionDrops returns a wssDialer where every session dials
// successfully but drops immediately with sessionErr (zero events processed).
func mockWSSDialerSessionDrops(sessionErr error) wssDialer {
	return func(ctx context.Context, wsURL string, input *rpc.EventSubscriptionInput) (*wssSession, error) {
		eventCh := make(chan *rpc.EmittedEventWithFinalityStatus, 1)
		errCh := make(chan error, 1)
		reorgCh := make(chan *client.ReorgEvent, 1)

		// Session connects but immediately errors (zero events).
		go func() {
			errCh <- sessionErr
		}()

		return &wssSession{
			events: eventCh,
			errs:   errCh,
			reorgs: reorgCh,
			close:  func() {},
		}, nil
	}
}

// mockWSSDialerEventsBeforeDrop returns a wssDialer where each session delivers
// the given events then drops with sessionErr. The counter tracks dial attempts.
// Uses an unbuffered event channel so each event is consumed by processWSSEvents
// before the error is sent — this prevents the select race where the error
// arrives before the event is read.
func mockWSSDialerEventsBeforeDrop(events []*rpc.EmittedEventWithFinalityStatus, sessionErr error, dialCount *atomic.Int32) wssDialer {
	return func(ctx context.Context, wsURL string, input *rpc.EventSubscriptionInput) (*wssSession, error) {
		dialCount.Add(1)
		eventCh := make(chan *rpc.EmittedEventWithFinalityStatus) // unbuffered
		errCh := make(chan error, 1)
		reorgCh := make(chan *client.ReorgEvent, 1)

		go func() {
			for _, e := range events {
				select {
				case eventCh <- e: // blocks until processWSSEvents reads
				case <-ctx.Done():
					return
				}
			}
			errCh <- sessionErr
		}()

		return &wssSession{
			events: eventCh,
			errs:   errCh,
			reorgs: reorgCh,
			close:  func() {},
		}, nil
	}
}

func TestSubscriberWSSSessionInstabilityFallback(t *testing.T) {
	// WSS dials succeed but sessions drop immediately (zero events) N times.
	// After maxWSSSessionFailures (5) consecutive drops, should fall back to polling.
	var pollCalls atomic.Int32

	server := mockRPCServer(t, map[string]func(json.RawMessage) (interface{}, error){
		"starknet_blockNumber": func(_ json.RawMessage) (interface{}, error) {
			return 105, nil
		},
		"starknet_getEvents": func(_ json.RawMessage) (interface{}, error) {
			n := pollCalls.Add(1)
			if n == 1 {
				return map[string]interface{}{
					"events": []map[string]interface{}{
						{
							"block_number":     100,
							"block_hash":       "0x1",
							"transaction_hash": "0x2",
							"from_address":     "0x3",
							"keys":             []string{"0x4"},
							"data":             []string{"0x5"},
						},
					},
				}, nil
			}
			return map[string]interface{}{"events": []interface{}{}}, nil
		},
	})
	defer server.Close()

	p, err := New(context.Background(), server.URL, nil)
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	events := make(chan RawEvent, 10)
	sub := p.NewSubscriber(
		[]ContractSubscription{{Address: newTestFelt(0xABC), StartBlock: 100}},
		events,
		&SubscriberConfig{BlocksPerQuery: 10},
	)

	// WSS dials succeed but sessions drop immediately with an error.
	sub.dialWSS = mockWSSDialerSessionDrops(fmt.Errorf("websocket: close 1013: Connection timeout exceeded"))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	go sub.Start(ctx)

	// Should eventually fall back to polling and deliver an event.
	select {
	case evt := <-events:
		if evt.BlockNumber != 100 {
			t.Errorf("polled event.BlockNumber = %d, want 100", evt.BlockNumber)
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for polled event after session instability fallback")
	}
}

func TestSubscriberWSSSessionHealthyResets(t *testing.T) {
	// WSS sessions that process events before dropping should reset the
	// consecutiveSessionFails counter, preventing premature fallback.
	var dialCount atomic.Int32

	server := mockRPCServer(t, map[string]func(json.RawMessage) (interface{}, error){
		"starknet_blockNumber": func(_ json.RawMessage) (interface{}, error) {
			return 1000, nil
		},
	})
	defer server.Close()

	p, err := New(context.Background(), server.URL, nil)
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	events := make(chan RawEvent, 100)
	sub := p.NewSubscriber(
		[]ContractSubscription{{Address: newTestFelt(0xABC), StartBlock: 100}},
		events,
		nil,
	)

	// Each session delivers 1 event then drops. Since events are processed,
	// the session failure counter resets each time → no fallback to polling.
	mockEvents := []*rpc.EmittedEventWithFinalityStatus{newTestEvent(200)}
	sub.dialWSS = mockWSSDialerEventsBeforeDrop(mockEvents, fmt.Errorf("connection reset"), &dialCount)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	go sub.Start(ctx)

	// Collect multiple events from repeated healthy sessions.
	for i := 0; i < 3; i++ {
		select {
		case evt := <-events:
			if evt.BlockNumber != 200 {
				t.Errorf("event[%d].BlockNumber = %d, want 200", i, evt.BlockNumber)
			}
		case <-ctx.Done():
			t.Fatalf("timed out waiting for event %d; only %d sessions dialed (expected no premature fallback)", i, dialCount.Load())
		}
	}

	// Verify multiple sessions were created (proving reconnection, not fallback).
	if dialCount.Load() < 3 {
		t.Errorf("expected at least 3 WSS dial attempts (reconnections), got %d", dialCount.Load())
	}
}

func TestSubscriberMultipleContracts(t *testing.T) {
	server := mockRPCServer(t, map[string]func(json.RawMessage) (interface{}, error){
		"starknet_blockNumber": func(_ json.RawMessage) (interface{}, error) {
			return 1000, nil
		},
	})
	defer server.Close()

	p, err := New(context.Background(), server.URL, nil)
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	events := make(chan RawEvent, 20)
	contracts := []ContractSubscription{
		{Address: newTestFelt(0xAAA), StartBlock: 100},
		{Address: newTestFelt(0xBBB), StartBlock: 200},
	}
	sub := p.NewSubscriber(contracts, events, nil)

	// Each contract gets its own mock dialer invocation.
	var dialCount atomic.Int32
	sub.dialWSS = func(ctx context.Context, wsURL string, input *rpc.EventSubscriptionInput) (*wssSession, error) {
		n := dialCount.Add(1)
		eventCh := make(chan *rpc.EmittedEventWithFinalityStatus, 5)
		errCh := make(chan error, 1)
		reorgCh := make(chan *client.ReorgEvent, 1)

		go func() {
			blockNum := uint64(100*n + 1)
			select {
			case eventCh <- newTestEvent(blockNum):
			case <-ctx.Done():
				return
			}
			<-ctx.Done()
		}()

		return &wssSession{
			events: eventCh,
			errs:   errCh,
			reorgs: reorgCh,
			close:  func() {},
		}, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	go sub.Start(ctx)

	// Should receive events from both contracts.
	received := 0
	for received < 2 {
		select {
		case <-events:
			received++
		case <-ctx.Done():
			t.Fatalf("timed out, received %d events, want 2", received)
		}
	}
}
