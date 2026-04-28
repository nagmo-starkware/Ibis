package api_test

import (
	"bufio"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/b-j-roberts/ibis/internal/api"
	"github.com/b-j-roberts/ibis/internal/config"
	"github.com/b-j-roberts/ibis/internal/engine"
	"github.com/b-j-roberts/ibis/internal/store"
	"github.com/b-j-roberts/ibis/internal/store/memory"
	"github.com/b-j-roberts/ibis/internal/types"
)

// setupFactoryTestServer creates an httptest.Server with a factory contract
// ("JediSwap"), two child contracts, shared event tables with test data,
// and an engine wired to the API server.
func setupFactoryTestServer(t *testing.T) (*httptest.Server, *api.EventBus) {
	t.Helper()

	st := memory.New()
	ctx := context.Background()

	// --- Shared event table schemas for factory children ---
	swapSchema := &types.TableSchema{
		Name:        "jediswap_swap",
		Contract:    "JediSwap",
		Event:       "Swap",
		TableType:   types.TableTypeLog,
		SharedTable: true,
		Columns: []types.Column{
			{Name: "block_number", Type: "uint64"},
			{Name: "transaction_hash", Type: "string"},
			{Name: "log_index", Type: "uint64"},
			{Name: "timestamp", Type: "uint64"},
			{Name: "contract_address", Type: "string"},
			{Name: "contract_name", Type: "string"},
			{Name: "event_name", Type: "string"},
			{Name: "status", Type: "string"},
			{Name: "sender", Type: "string"},
			{Name: "amount0", Type: "int64"},
		},
	}
	st.CreateTable(ctx, swapSchema)

	volumeSchema := &types.TableSchema{
		Name:        "jediswap_volume",
		Contract:    "JediSwap",
		Event:       "Volume",
		TableType:   types.TableTypeAggregation,
		SharedTable: true,
		Columns: []types.Column{
			{Name: "block_number", Type: "uint64"},
			{Name: "transaction_hash", Type: "string"},
			{Name: "log_index", Type: "uint64"},
			{Name: "timestamp", Type: "uint64"},
			{Name: "contract_address", Type: "string"},
			{Name: "contract_name", Type: "string"},
			{Name: "event_name", Type: "string"},
			{Name: "status", Type: "string"},
			{Name: "amount", Type: "int64"},
		},
		Aggregates: []types.AggregateSpec{
			{Column: "total_volume", Operation: "sum", Field: "amount"},
			{Column: "trade_count", Operation: "count", Field: "amount"},
		},
	}
	st.CreateTable(ctx, volumeSchema)

	// --- Engine with factory + child contracts ---
	eng := engine.New(
		&config.Config{Indexer: config.IndexerConfig{}},
		st, nil, slog.Default(),
	)

	// Factory contract.
	factorySchemas := map[string]*types.TableSchema{
		"Swap":   swapSchema,
		"Volume": volumeSchema,
	}
	eng.InjectContractForTest(&config.ContractConfig{
		Name:    "JediSwap",
		Address: "0xF001",
		Factories: []config.FactoryConfig{{
			Event:             "PairCreated",
			ChildAddressField: "pair",
			SharedTables:      true,
		}},
		Events: []config.EventConfig{
			{Name: "PairCreated", Table: config.TableConfig{Type: "log"}},
		},
	}, factorySchemas)

	// Child 1.
	eng.InjectContractForTest(&config.ContractConfig{
		Name:         "JediSwap_c001",
		Address:      "0xC001",
		FactoryName:  "JediSwap",
		FactoryMeta:  map[string]any{"token0": "0xETH", "token1": "0xUSDC"},
		StartBlock:   config.Uint64Ptr(100),
		SharedTables: true,
		Dynamic:      true,
	}, factorySchemas)

	// Child 2.
	eng.InjectContractForTest(&config.ContractConfig{
		Name:         "JediSwap_c002",
		Address:      "0xC002",
		FactoryName:  "JediSwap",
		FactoryMeta:  map[string]any{"token0": "0xDAI", "token1": "0xUSDC"},
		StartBlock:   config.Uint64Ptr(105),
		SharedTables: true,
		Dynamic:      true,
	}, factorySchemas)

	// --- Cursors ---
	st.SetCursor(ctx, "JediSwap", 110)
	st.SetCursor(ctx, "JediSwap_c001", 110)
	st.SetCursor(ctx, "JediSwap_c002", 108) // behind — simulates backfilling

	// --- Seed shared Swap table with events from both children ---
	swapOps := []store.Operation{
		{Type: store.OpInsert, Table: "jediswap_swap", Key: "100:0", BlockNumber: 100, LogIndex: 0,
			Data: map[string]any{
				"block_number": uint64(100), "log_index": uint64(0), "timestamp": uint64(1000),
				"transaction_hash": "0xaaa", "contract_address": "0xC001",
				"contract_name": "JediSwap_c001", "event_name": "Swap",
				"status": "ACCEPTED_L2", "sender": "0xalice", "amount0": int64(500),
			}},
		{Type: store.OpInsert, Table: "jediswap_swap", Key: "101:0", BlockNumber: 101, LogIndex: 0,
			Data: map[string]any{
				"block_number": uint64(101), "log_index": uint64(0), "timestamp": uint64(1010),
				"transaction_hash": "0xbbb", "contract_address": "0xC002",
				"contract_name": "JediSwap_c002", "event_name": "Swap",
				"status": "ACCEPTED_L2", "sender": "0xbob", "amount0": int64(300),
			}},
		{Type: store.OpInsert, Table: "jediswap_swap", Key: "102:0", BlockNumber: 102, LogIndex: 0,
			Data: map[string]any{
				"block_number": uint64(102), "log_index": uint64(0), "timestamp": uint64(1020),
				"transaction_hash": "0xccc", "contract_address": "0xC001",
				"contract_name": "JediSwap_c001", "event_name": "Swap",
				"status": "ACCEPTED_L2", "sender": "0xcharlie", "amount0": int64(200),
			}},
	}
	st.ApplyOperations(ctx, swapOps)

	// --- Seed shared Volume aggregation table ---
	volOps := []store.Operation{
		{Type: store.OpInsert, Table: "jediswap_volume", Key: "100:1", BlockNumber: 100, LogIndex: 1,
			Data: map[string]any{
				"block_number": uint64(100), "log_index": uint64(1), "timestamp": uint64(1000),
				"transaction_hash": "0xaaa", "contract_address": "0xC001",
				"contract_name": "JediSwap_c001", "event_name": "Volume",
				"status": "ACCEPTED_L2", "amount": int64(500),
			}},
		{Type: store.OpInsert, Table: "jediswap_volume", Key: "101:1", BlockNumber: 101, LogIndex: 1,
			Data: map[string]any{
				"block_number": uint64(101), "log_index": uint64(1), "timestamp": uint64(1010),
				"transaction_hash": "0xbbb", "contract_address": "0xC002",
				"contract_name": "JediSwap_c002", "event_name": "Volume",
				"status": "ACCEPTED_L2", "amount": int64(300),
			}},
	}
	st.ApplyOperations(ctx, volOps)

	// --- Event bus for SSE ---
	bus := api.NewEventBus()
	t.Cleanup(bus.Close)

	// --- Build API server ---
	srv := api.New(&api.ServerConfig{
		Store:   st,
		Schemas: []*types.TableSchema{swapSchema, volumeSchema},
		APIConfig: &config.APIConfig{
			Host: "localhost",
			Port: 8080,
		},
		Contracts: eng.AllContracts(),
		Logger:    slog.Default(),
		EventBus:  bus,
		Engine:    eng,
	})

	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, bus
}

func factoryGet(t *testing.T, ts *httptest.Server, path string) (code int, body map[string]any) {
	t.Helper()
	resp, err := http.Get(ts.URL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	json.NewDecoder(resp.Body).Decode(&body)
	return resp.StatusCode, body
}

// Test Plan Item 1: GET /v1/{factory}/children
func TestFactoryAPI_ListChildren(t *testing.T) {
	ts, _ := setupFactoryTestServer(t)

	status, body := factoryGet(t, ts, "/v1/JediSwap/children")
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d", status)
	}

	data := body["data"].([]any)
	if len(data) != 2 {
		t.Fatalf("expected 2 children, got %d", len(data))
	}

	count := int(body["count"].(float64))
	if count != 2 {
		t.Fatalf("expected count=2, got %d", count)
	}

	// Verify child structure includes required fields.
	child := data[0].(map[string]any)
	for _, f := range []string{"name", "address", "deployment_block", "current_block", "status", "events"} {
		if _, ok := child[f]; !ok {
			t.Errorf("child missing field %q", f)
		}
	}

	// Verify factory metadata is flattened into child entries.
	hasToken0, hasToken1 := false, false
	for _, item := range data {
		c := item.(map[string]any)
		if _, ok := c["token0"]; ok {
			hasToken0 = true
		}
		if _, ok := c["token1"]; ok {
			hasToken1 = true
		}
	}
	if !hasToken0 || !hasToken1 {
		t.Error("expected factory metadata (token0, token1) in child entries")
	}
}

// Test Plan Item 2: GET /v1/{factory}/children?token0=... — filter by metadata
func TestFactoryAPI_FilterChildrenByMetadata(t *testing.T) {
	ts, _ := setupFactoryTestServer(t)

	status, body := factoryGet(t, ts, "/v1/JediSwap/children?token0=0xETH")
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d", status)
	}

	data := body["data"].([]any)
	if len(data) != 1 {
		t.Fatalf("expected 1 child matching token0=0xETH, got %d", len(data))
	}

	child := data[0].(map[string]any)
	if child["token0"] != "0xETH" {
		t.Errorf("expected token0=0xETH, got %v", child["token0"])
	}
	if child["name"] != "JediSwap_c001" {
		t.Errorf("expected name=JediSwap_c001, got %v", child["name"])
	}
}

// Test Plan Item 3: GET /v1/{factory}/children/count
func TestFactoryAPI_ChildCount(t *testing.T) {
	ts, _ := setupFactoryTestServer(t)

	status, body := factoryGet(t, ts, "/v1/JediSwap/children/count")
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d", status)
	}

	count := int(body["count"].(float64))
	if count != 2 {
		t.Fatalf("expected count=2, got %d", count)
	}

	// Filtered count.
	status2, body2 := factoryGet(t, ts, "/v1/JediSwap/children/count?token0=0xETH")
	if status2 != http.StatusOK {
		t.Fatalf("expected 200, got %d", status2)
	}
	if int(body2["count"].(float64)) != 1 {
		t.Fatalf("expected filtered count=1, got %v", body2["count"])
	}
}

// Test Plan Item 4: per-child event query via ?contract_address=...
func TestFactoryAPI_PerChildEventQuery(t *testing.T) {
	ts, _ := setupFactoryTestServer(t)

	// All events (both children).
	status, body := factoryGet(t, ts, "/v1/JediSwap/Swap")
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d", status)
	}
	allData := body["data"].([]any)
	if len(allData) != 3 {
		t.Fatalf("expected 3 total swap events, got %d", len(allData))
	}

	// Per-child filter (no op prefix — tests default-to-eq).
	status2, body2 := factoryGet(t, ts, "/v1/JediSwap/Swap?contract_address=0xC001")
	if status2 != http.StatusOK {
		t.Fatalf("expected 200, got %d", status2)
	}
	filtered := body2["data"].([]any)
	if len(filtered) != 2 {
		t.Fatalf("expected 2 events from child1, got %d", len(filtered))
	}
	for _, item := range filtered {
		evt := item.(map[string]any)
		if evt["contract_address"] != "0xC001" {
			t.Errorf("expected contract_address=0xC001, got %v", evt["contract_address"])
		}
	}

	// With explicit eq. prefix.
	status3, body3 := factoryGet(t, ts, "/v1/JediSwap/Swap?contract_address=eq.0xC001")
	if status3 != http.StatusOK {
		t.Fatalf("expected 200, got %d", status3)
	}
	if len(body3["data"].([]any)) != 2 {
		t.Fatalf("expected 2 events with eq. prefix, got %d", len(body3["data"].([]any)))
	}
}

// Test Plan Item 5: cross-child aggregation
func TestFactoryAPI_CrossChildAggregation(t *testing.T) {
	ts, _ := setupFactoryTestServer(t)

	status, body := factoryGet(t, ts, "/v1/JediSwap/Volume/aggregate")
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d", status)
	}

	data := body["data"].(map[string]any)
	if data["total_volume"].(float64) != 800 {
		t.Errorf("expected total_volume=800, got %v", data["total_volume"])
	}
	if data["trade_count"].(float64) != 2 {
		t.Errorf("expected trade_count=2, got %v", data["trade_count"])
	}
}

// Test Plan Item 6: per-child aggregation accepts filter
func TestFactoryAPI_PerChildAggregationAcceptsFilter(t *testing.T) {
	ts, _ := setupFactoryTestServer(t)

	// Memory store ignores aggregate filters, but the API should accept them.
	status, _ := factoryGet(t, ts, "/v1/JediSwap/Volume/aggregate?contract_address=0xC001")
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d", status)
	}
}

// Test Plan Item 7: SSE streaming with per-child filter
func TestFactoryAPI_SSEStreamWithChildFilter(t *testing.T) {
	ts, _ := setupFactoryTestServer(t)

	req, _ := http.NewRequest("GET",
		ts.URL+"/v1/JediSwap/Swap/stream?contract_address=eq.0xC001", http.NoBody)
	sseCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req = req.WithContext(sseCtx)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("SSE connect: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Errorf("expected text/event-stream, got %q", resp.Header.Get("Content-Type"))
	}
}

// Test Plan Item 7b: SSE event delivery filtering
func TestFactoryAPI_SSEEventDeliveryFiltered(t *testing.T) {
	ts, bus := setupFactoryTestServer(t)

	req, _ := http.NewRequest("GET",
		ts.URL+"/v1/JediSwap/Swap/stream?contract_address=eq.0xC001", http.NoBody)
	sseCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req = req.WithContext(sseCtx)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("SSE connect: %v", err)
	}
	defer resp.Body.Close()

	go func() {
		time.Sleep(50 * time.Millisecond)
		// Event from child2 — should be filtered out.
		bus.Publish(api.StreamEvent{
			Table: "jediswap_swap", Contract: "JediSwap", Event: "Swap",
			BlockNumber: 200, LogIndex: 0,
			Data: map[string]any{"contract_address": "0xC002", "sender": "0xbob"},
		})
		time.Sleep(10 * time.Millisecond)
		// Event from child1 — should be delivered.
		bus.Publish(api.StreamEvent{
			Table: "jediswap_swap", Contract: "JediSwap", Event: "Swap",
			BlockNumber: 201, LogIndex: 0,
			Data: map[string]any{"contract_address": "0xC001", "sender": "0xalice"},
		})
	}()

	scanner := bufio.NewScanner(resp.Body)
	var received []map[string]any
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "data: ") {
			var data map[string]any
			json.Unmarshal([]byte(line[6:]), &data)
			received = append(received, data)
			break
		}
	}

	if len(received) == 0 {
		t.Fatal("expected at least 1 SSE event")
	}
	if received[0]["contract_address"] != "0xC001" {
		t.Errorf("expected SSE event from 0xC001, got %v", received[0]["contract_address"])
	}
}

// Test Plan Item 8: status endpoint includes factory summary
func TestFactoryAPI_StatusIncludesFactorySummary(t *testing.T) {
	ts, _ := setupFactoryTestServer(t)

	status, body := factoryGet(t, ts, "/v1/status")
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d", status)
	}

	factories, ok := body["factories"]
	if !ok {
		t.Fatal("expected 'factories' in status response")
	}

	factoryMap := factories.(map[string]any)
	jediswap, ok := factoryMap["JediSwap"]
	if !ok {
		t.Fatal("expected 'JediSwap' in factories")
	}

	info := jediswap.(map[string]any)
	if int(info["child_count"].(float64)) != 2 {
		t.Errorf("expected child_count=2, got %v", info["child_count"])
	}
	// child1 cursor=110 >= global(108), child2 cursor=108 >= global(108)
	// Global cursor = min(110, 110, 108) = 108
	// child1: 110 >= 108 → synced
	// child2: 108 >= 108 → synced
	synced := int(info["synced"].(float64))
	backfilling := int(info["backfilling"].(float64))
	if synced+backfilling != 2 {
		t.Errorf("expected synced+backfilling=2, got %d+%d", synced, backfilling)
	}
}

// Test Plan Item 9: non-existent factory returns 404
func TestFactoryAPI_NonExistentFactory(t *testing.T) {
	ts, _ := setupFactoryTestServer(t)

	status, body := factoryGet(t, ts, "/v1/nonexistent/children")
	if status != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", status)
	}
	errMsg := body["error"].(string)
	if !strings.Contains(errMsg, "factory not found") {
		t.Errorf("expected 'factory not found' error, got %q", errMsg)
	}

	status2, _ := factoryGet(t, ts, "/v1/nonexistent/children/count")
	if status2 != http.StatusNotFound {
		t.Fatalf("expected 404 for count, got %d", status2)
	}
}

// --- Pagination & Sorting Tests (Task 3.23) ---

// Test: default response includes total, limit, offset fields
func TestFactoryAPI_PaginationEnvelope(t *testing.T) {
	ts, _ := setupFactoryTestServer(t)

	status, body := factoryGet(t, ts, "/v1/JediSwap/children")
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d", status)
	}

	// Verify envelope fields.
	for _, field := range []string{"data", "count", "total", "limit", "offset"} {
		if _, ok := body[field]; !ok {
			t.Errorf("response missing field %q", field)
		}
	}

	total := int(body["total"].(float64))
	count := int(body["count"].(float64))
	limit := int(body["limit"].(float64))
	offset := int(body["offset"].(float64))

	if total != 2 {
		t.Errorf("expected total=2, got %d", total)
	}
	if count != 2 {
		t.Errorf("expected count=2, got %d", count)
	}
	if limit != 50 { // defaultLimit
		t.Errorf("expected limit=50, got %d", limit)
	}
	if offset != 0 {
		t.Errorf("expected offset=0, got %d", offset)
	}
}

// Test: limit restricts page size
func TestFactoryAPI_PaginationLimit(t *testing.T) {
	ts, _ := setupFactoryTestServer(t)

	status, body := factoryGet(t, ts, "/v1/JediSwap/children?limit=1")
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d", status)
	}

	data := body["data"].([]any)
	if len(data) != 1 {
		t.Fatalf("expected 1 child in page, got %d", len(data))
	}
	if int(body["count"].(float64)) != 1 {
		t.Errorf("expected count=1, got %v", body["count"])
	}
	if int(body["total"].(float64)) != 2 {
		t.Errorf("expected total=2, got %v", body["total"])
	}
}

// Test: offset skips entries
func TestFactoryAPI_PaginationOffset(t *testing.T) {
	ts, _ := setupFactoryTestServer(t)

	status, body := factoryGet(t, ts, "/v1/JediSwap/children?limit=1&offset=1")
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d", status)
	}

	data := body["data"].([]any)
	if len(data) != 1 {
		t.Fatalf("expected 1 child in page, got %d", len(data))
	}
	if int(body["total"].(float64)) != 2 {
		t.Errorf("expected total=2, got %v", body["total"])
	}
}

// Test: offset beyond total returns empty data
func TestFactoryAPI_PaginationOffsetBeyondTotal(t *testing.T) {
	ts, _ := setupFactoryTestServer(t)

	status, body := factoryGet(t, ts, "/v1/JediSwap/children?offset=100")
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d", status)
	}

	data := body["data"].([]any)
	if len(data) != 0 {
		t.Fatalf("expected 0 children when offset > total, got %d", len(data))
	}
	if int(body["count"].(float64)) != 0 {
		t.Errorf("expected count=0, got %v", body["count"])
	}
	if int(body["total"].(float64)) != 2 {
		t.Errorf("expected total=2, got %v", body["total"])
	}
}

// Test: default sort is deployment_block.desc (newest first)
func TestFactoryAPI_DefaultSortOrder(t *testing.T) {
	ts, _ := setupFactoryTestServer(t)

	status, body := factoryGet(t, ts, "/v1/JediSwap/children")
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d", status)
	}

	data := body["data"].([]any)
	if len(data) != 2 {
		t.Fatalf("expected 2 children, got %d", len(data))
	}

	// c002 has start_block=105, c001 has start_block=100.
	// desc order: c002 first, c001 second.
	first := data[0].(map[string]any)
	second := data[1].(map[string]any)
	if first["name"] != "JediSwap_c002" {
		t.Errorf("expected first child to be c002 (block 105), got %v", first["name"])
	}
	if second["name"] != "JediSwap_c001" {
		t.Errorf("expected second child to be c001 (block 100), got %v", second["name"])
	}
}

// Test: sort by deployment_block.asc
func TestFactoryAPI_SortByDeploymentBlockAsc(t *testing.T) {
	ts, _ := setupFactoryTestServer(t)

	status, body := factoryGet(t, ts, "/v1/JediSwap/children?order=deployment_block.asc")
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d", status)
	}

	data := body["data"].([]any)
	first := data[0].(map[string]any)
	second := data[1].(map[string]any)
	if first["name"] != "JediSwap_c001" {
		t.Errorf("expected first child c001 (block 100) in asc order, got %v", first["name"])
	}
	if second["name"] != "JediSwap_c002" {
		t.Errorf("expected second child c002 (block 105) in asc order, got %v", second["name"])
	}
}

// Test: sort by name.asc
func TestFactoryAPI_SortByName(t *testing.T) {
	ts, _ := setupFactoryTestServer(t)

	status, body := factoryGet(t, ts, "/v1/JediSwap/children?order=name.asc")
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d", status)
	}

	data := body["data"].([]any)
	first := data[0].(map[string]any)
	if first["name"] != "JediSwap_c001" {
		t.Errorf("expected first child c001 in name.asc order, got %v", first["name"])
	}
}

// Test: sort by status
func TestFactoryAPI_SortByStatus(t *testing.T) {
	ts, _ := setupFactoryTestServer(t)

	// Both children have same status, so just verify no error.
	status, _ := factoryGet(t, ts, "/v1/JediSwap/children?order=status.asc")
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d", status)
	}
}

// Test: combined filter + pagination
func TestFactoryAPI_FilterWithPagination(t *testing.T) {
	ts, _ := setupFactoryTestServer(t)

	// Filter to 1 child + limit=1 → count=1, total=1
	status, body := factoryGet(t, ts, "/v1/JediSwap/children?token0=0xETH&limit=1")
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d", status)
	}

	data := body["data"].([]any)
	if len(data) != 1 {
		t.Fatalf("expected 1 child, got %d", len(data))
	}
	if int(body["total"].(float64)) != 1 {
		t.Errorf("expected total=1 after filter, got %v", body["total"])
	}
	child := data[0].(map[string]any)
	if child["token0"] != "0xETH" {
		t.Errorf("expected token0=0xETH, got %v", child["token0"])
	}
}

// Test: empty result when filter matches nothing
func TestFactoryAPI_EmptyFilterResult(t *testing.T) {
	ts, _ := setupFactoryTestServer(t)

	status, body := factoryGet(t, ts, "/v1/JediSwap/children?token0=0xNONEXISTENT")
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d", status)
	}

	data := body["data"].([]any)
	if len(data) != 0 {
		t.Fatalf("expected 0 children, got %d", len(data))
	}
	if int(body["total"].(float64)) != 0 {
		t.Errorf("expected total=0, got %v", body["total"])
	}
	if int(body["count"].(float64)) != 0 {
		t.Errorf("expected count=0, got %v", body["count"])
	}
}

// Test: sort by metadata field
func TestFactoryAPI_SortByMetadataField(t *testing.T) {
	ts, _ := setupFactoryTestServer(t)

	status, body := factoryGet(t, ts, "/v1/JediSwap/children?order=token0.asc")
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d", status)
	}

	data := body["data"].([]any)
	// 0xDAI < 0xETH alphabetically.
	first := data[0].(map[string]any)
	if first["token0"] != "0xDAI" {
		t.Errorf("expected first child to have token0=0xDAI in asc order, got %v", first["token0"])
	}
}

// Test: filter defaults to eq
func TestFactoryAPI_FilterDefaultsToEq(t *testing.T) {
	ts, _ := setupFactoryTestServer(t)

	// Without operator prefix — should work as equality filter.
	status, body := factoryGet(t, ts, "/v1/JediSwap/Swap?sender=0xalice")
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d", status)
	}

	data := body["data"].([]any)
	if len(data) != 1 {
		t.Fatalf("expected 1 event from 0xalice, got %d", len(data))
	}
	if data[0].(map[string]any)["sender"] != "0xalice" {
		t.Errorf("expected sender=0xalice, got %v", data[0].(map[string]any)["sender"])
	}
}
