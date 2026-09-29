package memory

import (
	"context"
	"testing"

	"github.com/b-j-roberts/ibis/internal/config"
	"github.com/b-j-roberts/ibis/internal/store"
	"github.com/b-j-roberts/ibis/internal/types"
)

func newTestStore(t *testing.T) *MemoryStore {
	t.Helper()
	return New()
}

func TestInsertAndGetEvents(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	s.CreateTable(ctx, &types.TableSchema{
		Name:      "transfers",
		TableType: types.TableTypeLog,
	})

	ops := []store.Operation{
		{
			Type:        store.OpInsert,
			Table:       "transfers",
			BlockNumber: 100,
			LogIndex:    0,
			Data: map[string]any{
				"from":         "0xabc",
				"to":           "0xdef",
				"amount":       1000,
				"block_number": uint64(100),
				"log_index":    uint64(0),
			},
		},
		{
			Type:        store.OpInsert,
			Table:       "transfers",
			BlockNumber: 101,
			LogIndex:    0,
			Data: map[string]any{
				"from":         "0xdef",
				"to":           "0x123",
				"amount":       500,
				"block_number": uint64(101),
				"log_index":    uint64(0),
			},
		},
	}

	if err := s.ApplyOperations(ctx, ops); err != nil {
		t.Fatalf("apply operations: %v", err)
	}

	events, err := s.GetEvents(ctx, "transfers", store.Query{Limit: 10})
	if err != nil {
		t.Fatalf("get events: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(events))
	}

	// Ascending order by default: block 100 first.
	if events[0].BlockNumber != 100 {
		t.Errorf("expected first event at block 100, got %d", events[0].BlockNumber)
	}
	if events[1].BlockNumber != 101 {
		t.Errorf("expected second event at block 101, got %d", events[1].BlockNumber)
	}
}

func TestDescendingOrder(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	s.CreateTable(ctx, &types.TableSchema{Name: "events", TableType: types.TableTypeLog})

	for i := uint64(0); i < 5; i++ {
		s.ApplyOperations(ctx, []store.Operation{{
			Type:        store.OpInsert,
			Table:       "events",
			BlockNumber: 100 + i,
			LogIndex:    0,
			Data: map[string]any{
				"block_number": 100 + i,
				"value":        i,
			},
		}})
	}

	events, err := s.GetEvents(ctx, "events", store.Query{
		Limit:    10,
		OrderDir: store.OrderDesc,
	})
	if err != nil {
		t.Fatalf("get events desc: %v", err)
	}
	if len(events) != 5 {
		t.Fatalf("expected 5 events, got %d", len(events))
	}
	// Descending: block 104 first.
	if events[0].BlockNumber != 104 {
		t.Errorf("expected first event at block 104, got %d", events[0].BlockNumber)
	}
	if events[4].BlockNumber != 100 {
		t.Errorf("expected last event at block 100, got %d", events[4].BlockNumber)
	}
}

func TestPagination(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	s.CreateTable(ctx, &types.TableSchema{Name: "logs", TableType: types.TableTypeLog})

	for i := uint64(0); i < 10; i++ {
		s.ApplyOperations(ctx, []store.Operation{{
			Type:        store.OpInsert,
			Table:       "logs",
			BlockNumber: i,
			LogIndex:    0,
			Data:        map[string]any{"block_number": i},
		}})
	}

	// Get page 1: offset 0, limit 3.
	page1, err := s.GetEvents(ctx, "logs", store.Query{Limit: 3, Offset: 0})
	if err != nil {
		t.Fatalf("get page 1: %v", err)
	}
	if len(page1) != 3 {
		t.Fatalf("expected 3 events on page 1, got %d", len(page1))
	}

	// Get page 2: offset 3, limit 3.
	page2, err := s.GetEvents(ctx, "logs", store.Query{Limit: 3, Offset: 3})
	if err != nil {
		t.Fatalf("get page 2: %v", err)
	}
	if len(page2) != 3 {
		t.Fatalf("expected 3 events on page 2, got %d", len(page2))
	}

	// Ensure no overlap.
	if page1[2].BlockNumber == page2[0].BlockNumber {
		t.Error("page overlap detected")
	}
}

func TestFiltering(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	s.CreateTable(ctx, &types.TableSchema{Name: "trades", TableType: types.TableTypeLog})

	s.ApplyOperations(ctx, []store.Operation{
		{
			Type: store.OpInsert, Table: "trades", BlockNumber: 1, LogIndex: 0,
			Data: map[string]any{"pair": "ETH/USDC", "amount": 100, "block_number": uint64(1)},
		},
		{
			Type: store.OpInsert, Table: "trades", BlockNumber: 2, LogIndex: 0,
			Data: map[string]any{"pair": "BTC/USDC", "amount": 200, "block_number": uint64(2)},
		},
		{
			Type: store.OpInsert, Table: "trades", BlockNumber: 3, LogIndex: 0,
			Data: map[string]any{"pair": "ETH/USDC", "amount": 300, "block_number": uint64(3)},
		},
	})

	// Filter by pair == ETH/USDC.
	events, err := s.GetEvents(ctx, "trades", store.Query{
		Limit:   10,
		Filters: []store.Filter{{Field: "pair", Operator: "eq", Value: "ETH/USDC"}},
	})
	if err != nil {
		t.Fatalf("get filtered events: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("expected 2 ETH/USDC events, got %d", len(events))
	}

	// Filter by amount > 150.
	events, err = s.GetEvents(ctx, "trades", store.Query{
		Limit:   10,
		Filters: []store.Filter{{Field: "amount", Operator: "gt", Value: 150}},
	})
	if err != nil {
		t.Fatalf("get gt filtered events: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("expected 2 events with amount > 150, got %d", len(events))
	}
}

func TestDeleteOperation(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	s.CreateTable(ctx, &types.TableSchema{Name: "events", TableType: types.TableTypeLog})

	s.ApplyOperations(ctx, []store.Operation{{
		Type: store.OpInsert, Table: "events", BlockNumber: 10, LogIndex: 0,
		Data: map[string]any{"value": "hello"},
	}})

	events, _ := s.GetEvents(ctx, "events", store.Query{Limit: 10})
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}

	// Delete it.
	s.ApplyOperations(ctx, []store.Operation{{
		Type: store.OpDelete, Table: "events", BlockNumber: 10, LogIndex: 0,
		Data: map[string]any{"value": "hello"},
	}})

	events, _ = s.GetEvents(ctx, "events", store.Query{Limit: 10})
	if len(events) != 0 {
		t.Fatalf("expected 0 events after delete, got %d", len(events))
	}
}

func TestRevertOperations(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	s.CreateTable(ctx, &types.TableSchema{Name: "events", TableType: types.TableTypeLog})

	ops := []store.Operation{
		{
			Type: store.OpInsert, Table: "events", BlockNumber: 50, LogIndex: 0,
			Data: map[string]any{"value": "a", "block_number": uint64(50)},
		},
		{
			Type: store.OpInsert, Table: "events", BlockNumber: 50, LogIndex: 1,
			Data: map[string]any{"value": "b", "block_number": uint64(50)},
		},
	}

	s.ApplyOperations(ctx, ops)

	events, _ := s.GetEvents(ctx, "events", store.Query{Limit: 10})
	if len(events) != 2 {
		t.Fatalf("expected 2 events before revert, got %d", len(events))
	}

	// Revert the operations.
	if err := s.RevertOperations(ctx, ops); err != nil {
		t.Fatalf("revert operations: %v", err)
	}

	events, _ = s.GetEvents(ctx, "events", store.Query{Limit: 10})
	if len(events) != 0 {
		t.Fatalf("expected 0 events after revert, got %d", len(events))
	}
}

func TestRevertUpdate(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	s.CreateTable(ctx, &types.TableSchema{Name: "events", TableType: types.TableTypeLog})

	// Insert initial.
	s.ApplyOperations(ctx, []store.Operation{{
		Type: store.OpInsert, Table: "events", BlockNumber: 10, LogIndex: 0,
		Data: map[string]any{"value": "original"},
	}})

	// Update with revert data.
	updateOp := store.Operation{
		Type: store.OpUpdate, Table: "events", BlockNumber: 10, LogIndex: 0,
		Data: map[string]any{"value": "updated"},
		Prev: map[string]any{"value": "original"},
	}
	s.ApplyOperations(ctx, []store.Operation{updateOp})

	events, _ := s.GetEvents(ctx, "events", store.Query{Limit: 10})
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Data["value"] != "updated" {
		t.Errorf("expected updated value, got %v", events[0].Data["value"])
	}

	// Revert the update — should restore original.
	s.RevertOperations(ctx, []store.Operation{updateOp})

	events, _ = s.GetEvents(ctx, "events", store.Query{Limit: 10})
	if len(events) != 1 {
		t.Fatalf("expected 1 event after revert, got %d", len(events))
	}
	if events[0].Data["value"] != "original" {
		t.Errorf("expected original value after revert, got %v", events[0].Data["value"])
	}
}

func TestUniqueTable(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	s.CreateTable(ctx, &types.TableSchema{
		Name:      "leaderboard",
		TableType: types.TableTypeUnique,
		UniqueKey: "trader",
	})

	// Insert two entries for the same trader — unique should keep last.
	s.ApplyOperations(ctx, []store.Operation{
		{
			Type: store.OpInsert, Table: "leaderboard", BlockNumber: 1, LogIndex: 0,
			Data: map[string]any{"trader": "alice", "score": 100, "block_number": uint64(1)},
		},
	})
	s.ApplyOperations(ctx, []store.Operation{
		{
			Type: store.OpInsert, Table: "leaderboard", BlockNumber: 2, LogIndex: 0,
			Data: map[string]any{"trader": "alice", "score": 200, "block_number": uint64(2)},
		},
	})
	// Insert different trader.
	s.ApplyOperations(ctx, []store.Operation{
		{
			Type: store.OpInsert, Table: "leaderboard", BlockNumber: 3, LogIndex: 0,
			Data: map[string]any{"trader": "bob", "score": 150, "block_number": uint64(3)},
		},
	})

	events, err := s.GetUniqueEvents(ctx, "leaderboard", store.Query{Limit: 10})
	if err != nil {
		t.Fatalf("get unique events: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("expected 2 unique entries, got %d", len(events))
	}

	// Find alice's entry — should have score 200.
	for _, evt := range events {
		if evt.Data["trader"] == "alice" {
			if score, ok := evt.Data["score"]; !ok || toFloat64(score) != 200 {
				t.Errorf("expected alice score 200, got %v", score)
			}
		}
	}
}

func TestAggregation(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	s.CreateTable(ctx, &types.TableSchema{
		Name:      "volume",
		TableType: types.TableTypeAggregation,
		Aggregates: []types.AggregateSpec{
			{Column: "total_volume", Operation: "sum", Field: "amount"},
			{Column: "trade_count", Operation: "count"},
		},
	})

	s.ApplyOperations(ctx, []store.Operation{
		{
			Type: store.OpInsert, Table: "volume", BlockNumber: 1, LogIndex: 0,
			Data: map[string]any{"amount": 100.0},
		},
		{
			Type: store.OpInsert, Table: "volume", BlockNumber: 2, LogIndex: 0,
			Data: map[string]any{"amount": 250.0},
		},
		{
			Type: store.OpInsert, Table: "volume", BlockNumber: 3, LogIndex: 0,
			Data: map[string]any{"amount": 50.0},
		},
	})

	result, err := s.GetAggregation(ctx, "volume", store.Query{})
	if err != nil {
		t.Fatalf("get aggregation: %v", err)
	}

	totalVol := toFloat64(result.Values["total_volume"])
	if totalVol != 400.0 {
		t.Errorf("expected total_volume 400, got %v", totalVol)
	}

	tradeCount := toFloat64(result.Values["trade_count"])
	if tradeCount != 3.0 {
		t.Errorf("expected trade_count 3, got %v", tradeCount)
	}
}

func TestAggregationRevert(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	s.CreateTable(ctx, &types.TableSchema{
		Name:      "volume",
		TableType: types.TableTypeAggregation,
		Aggregates: []types.AggregateSpec{
			{Column: "total", Operation: "sum", Field: "amount"},
			{Column: "count", Operation: "count"},
		},
	})

	ops := []store.Operation{
		{
			Type: store.OpInsert, Table: "volume", BlockNumber: 5, LogIndex: 0,
			Data: map[string]any{"amount": 100.0},
		},
		{
			Type: store.OpInsert, Table: "volume", BlockNumber: 5, LogIndex: 1,
			Data: map[string]any{"amount": 200.0},
		},
	}

	s.ApplyOperations(ctx, ops)

	result, _ := s.GetAggregation(ctx, "volume", store.Query{})
	if toFloat64(result.Values["total"]) != 300.0 {
		t.Fatalf("expected total 300 before revert, got %v", result.Values["total"])
	}

	// Revert.
	s.RevertOperations(ctx, ops)

	result, _ = s.GetAggregation(ctx, "volume", store.Query{})
	if toFloat64(result.Values["total"]) != 0.0 {
		t.Errorf("expected total 0 after revert, got %v", result.Values["total"])
	}
	if toFloat64(result.Values["count"]) != 0.0 {
		t.Errorf("expected count 0 after revert, got %v", result.Values["count"])
	}
}

func TestAggregationAvg(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	s.CreateTable(ctx, &types.TableSchema{
		Name:      "stats",
		TableType: types.TableTypeAggregation,
		Aggregates: []types.AggregateSpec{
			{Column: "avg_score", Operation: "avg", Field: "score"},
		},
	})

	s.ApplyOperations(ctx, []store.Operation{
		{Type: store.OpInsert, Table: "stats", BlockNumber: 1, LogIndex: 0, Data: map[string]any{"score": 10.0}},
		{Type: store.OpInsert, Table: "stats", BlockNumber: 2, LogIndex: 0, Data: map[string]any{"score": 20.0}},
		{Type: store.OpInsert, Table: "stats", BlockNumber: 3, LogIndex: 0, Data: map[string]any{"score": 30.0}},
	})

	result, _ := s.GetAggregation(ctx, "stats", store.Query{})
	avg := toFloat64(result.Values["avg_score"])
	if avg != 20.0 {
		t.Errorf("expected avg 20, got %v", avg)
	}
}

func TestCursor(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// Initially 0.
	cursor, err := s.GetCursor(ctx, "mycontract")
	if err != nil {
		t.Fatalf("get cursor: %v", err)
	}
	if cursor != 0 {
		t.Errorf("expected initial cursor 0, got %d", cursor)
	}

	// Set cursor.
	if err := s.SetCursor(ctx, "mycontract", 12345); err != nil {
		t.Fatalf("set cursor: %v", err)
	}

	cursor, err = s.GetCursor(ctx, "mycontract")
	if err != nil {
		t.Fatalf("get cursor after set: %v", err)
	}
	if cursor != 12345 {
		t.Errorf("expected cursor 12345, got %d", cursor)
	}

	// Update cursor.
	s.SetCursor(ctx, "mycontract", 99999)
	cursor, _ = s.GetCursor(ctx, "mycontract")
	if cursor != 99999 {
		t.Errorf("expected cursor 99999, got %d", cursor)
	}
}

func TestPerContractCursors(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	s.SetCursor(ctx, "contract_a", 100)
	s.SetCursor(ctx, "contract_b", 200)

	cursorA, _ := s.GetCursor(ctx, "contract_a")
	cursorB, _ := s.GetCursor(ctx, "contract_b")
	if cursorA != 100 {
		t.Errorf("expected contract_a cursor 100, got %d", cursorA)
	}
	if cursorB != 200 {
		t.Errorf("expected contract_b cursor 200, got %d", cursorB)
	}

	// GetAllCursors.
	all, err := s.GetAllCursors(ctx)
	if err != nil {
		t.Fatalf("get all cursors: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("expected 2 cursors, got %d", len(all))
	}
	if all["contract_a"] != 100 || all["contract_b"] != 200 {
		t.Errorf("unexpected cursors: %v", all)
	}
}

func TestCreateAndMigrateTable(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	schema := types.TableSchema{
		Name:      "test_table",
		Contract:  "TestContract",
		Event:     "Transfer",
		TableType: types.TableTypeLog,
		Columns: []types.Column{
			{Name: "from", Type: "string"},
			{Name: "to", Type: "string"},
		},
	}

	if err := s.CreateTable(ctx, &schema); err != nil {
		t.Fatalf("create table: %v", err)
	}

	// Verify schema is stored.
	s.mu.RLock()
	stored, ok := s.schemas["test_table"]
	s.mu.RUnlock()
	if !ok {
		t.Fatal("schema not found after create")
	}
	if stored.Event != "Transfer" {
		t.Errorf("expected event Transfer, got %s", stored.Event)
	}

	// Migrate: add a column.
	schema.Columns = append(schema.Columns, types.Column{Name: "amount", Type: "int64"})
	if err := s.MigrateTable(ctx, &schema); err != nil {
		t.Fatalf("migrate table: %v", err)
	}

	s.mu.RLock()
	stored = s.schemas["test_table"]
	s.mu.RUnlock()
	if len(stored.Columns) != 3 {
		t.Errorf("expected 3 columns after migration, got %d", len(stored.Columns))
	}
}

func TestEmptyTableReturnsNoEvents(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	s.CreateTable(ctx, &types.TableSchema{Name: "empty", TableType: types.TableTypeLog})

	events, err := s.GetEvents(ctx, "empty", store.Query{Limit: 10})
	if err != nil {
		t.Fatalf("get events from empty table: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("expected 0 events from empty table, got %d", len(events))
	}
}

func TestMultipleLogIndicesInSameBlock(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	s.CreateTable(ctx, &types.TableSchema{Name: "events", TableType: types.TableTypeLog})

	s.ApplyOperations(ctx, []store.Operation{
		{
			Type: store.OpInsert, Table: "events", BlockNumber: 100, LogIndex: 0,
			Data: map[string]any{"value": "first", "block_number": uint64(100), "log_index": uint64(0)},
		},
		{
			Type: store.OpInsert, Table: "events", BlockNumber: 100, LogIndex: 1,
			Data: map[string]any{"value": "second", "block_number": uint64(100), "log_index": uint64(1)},
		},
		{
			Type: store.OpInsert, Table: "events", BlockNumber: 100, LogIndex: 2,
			Data: map[string]any{"value": "third", "block_number": uint64(100), "log_index": uint64(2)},
		},
	})

	events, err := s.GetEvents(ctx, "events", store.Query{Limit: 10})
	if err != nil {
		t.Fatalf("get events: %v", err)
	}
	if len(events) != 3 {
		t.Fatalf("expected 3 events in same block, got %d", len(events))
	}

	// Verify ordering by log index within same block.
	if events[0].LogIndex != 0 || events[1].LogIndex != 1 || events[2].LogIndex != 2 {
		t.Error("events not ordered by log index within block")
	}
}

func TestFilterOperators(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	s.CreateTable(ctx, &types.TableSchema{Name: "data", TableType: types.TableTypeLog})

	for i := 0; i < 5; i++ {
		s.ApplyOperations(ctx, []store.Operation{{
			Type: store.OpInsert, Table: "data", BlockNumber: uint64(i), LogIndex: 0,
			Data: map[string]any{"score": i * 10, "block_number": uint64(i)},
		}})
	}

	tests := []struct {
		name     string
		filter   store.Filter
		expected int
	}{
		{"eq", store.Filter{Field: "score", Operator: "eq", Value: 20}, 1},
		{"neq", store.Filter{Field: "score", Operator: "neq", Value: 20}, 4},
		{"gt", store.Filter{Field: "score", Operator: "gt", Value: 20}, 2},
		{"gte", store.Filter{Field: "score", Operator: "gte", Value: 20}, 3},
		{"lt", store.Filter{Field: "score", Operator: "lt", Value: 20}, 2},
		{"lte", store.Filter{Field: "score", Operator: "lte", Value: 20}, 3},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			events, err := s.GetEvents(ctx, "data", store.Query{
				Limit:   10,
				Filters: []store.Filter{tt.filter},
			})
			if err != nil {
				t.Fatalf("get filtered events: %v", err)
			}
			if len(events) != tt.expected {
				t.Errorf("operator %s: expected %d events, got %d", tt.name, tt.expected, len(events))
			}
		})
	}
}

func TestNonExistentTableReturnsNil(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	events, err := s.GetEvents(ctx, "nonexistent", store.Query{Limit: 10})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if events != nil {
		t.Errorf("expected nil for nonexistent table, got %v", events)
	}

	unique, err := s.GetUniqueEvents(ctx, "nonexistent", store.Query{Limit: 10})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if unique != nil {
		t.Errorf("expected nil for nonexistent unique table, got %v", unique)
	}

	agg, err := s.GetAggregation(ctx, "nonexistent", store.Query{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(agg.Values) != 0 {
		t.Errorf("expected empty aggregation, got %v", agg.Values)
	}
}

func TestCloseIsNoop(t *testing.T) {
	s := newTestStore(t)
	if err := s.Close(); err != nil {
		t.Fatalf("close should succeed: %v", err)
	}
}

func TestDataIsolation(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	s.CreateTable(ctx, &types.TableSchema{Name: "t1", TableType: types.TableTypeLog})
	s.CreateTable(ctx, &types.TableSchema{Name: "t2", TableType: types.TableTypeLog})

	s.ApplyOperations(ctx, []store.Operation{
		{Type: store.OpInsert, Table: "t1", BlockNumber: 1, LogIndex: 0, Data: map[string]any{"v": "a"}},
		{Type: store.OpInsert, Table: "t2", BlockNumber: 1, LogIndex: 0, Data: map[string]any{"v": "b"}},
		{Type: store.OpInsert, Table: "t2", BlockNumber: 2, LogIndex: 0, Data: map[string]any{"v": "c"}},
	})

	e1, _ := s.GetEvents(ctx, "t1", store.Query{Limit: 10})
	if len(e1) != 1 {
		t.Errorf("expected 1 event in t1, got %d", len(e1))
	}

	e2, _ := s.GetEvents(ctx, "t2", store.Query{Limit: 10})
	if len(e2) != 2 {
		t.Errorf("expected 2 events in t2, got %d", len(e2))
	}
}

func TestUniqueTableFiltering(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	s.CreateTable(ctx, &types.TableSchema{
		Name:      "scores",
		TableType: types.TableTypeUnique,
		UniqueKey: "player",
	})

	s.ApplyOperations(ctx, []store.Operation{
		{Type: store.OpInsert, Table: "scores", BlockNumber: 1, LogIndex: 0,
			Data: map[string]any{"player": "alice", "score": 100, "block_number": uint64(1)}},
		{Type: store.OpInsert, Table: "scores", BlockNumber: 2, LogIndex: 0,
			Data: map[string]any{"player": "bob", "score": 200, "block_number": uint64(2)}},
		{Type: store.OpInsert, Table: "scores", BlockNumber: 3, LogIndex: 0,
			Data: map[string]any{"player": "carol", "score": 50, "block_number": uint64(3)}},
	})

	// Filter unique events by score > 75.
	events, err := s.GetUniqueEvents(ctx, "scores", store.Query{
		Limit:   10,
		Filters: []store.Filter{{Field: "score", Operator: "gt", Value: 75}},
	})
	if err != nil {
		t.Fatalf("get filtered unique events: %v", err)
	}
	if len(events) != 2 {
		t.Errorf("expected 2 unique events with score > 75, got %d", len(events))
	}
}

func TestPaginationBeyondResults(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	s.CreateTable(ctx, &types.TableSchema{Name: "small", TableType: types.TableTypeLog})

	s.ApplyOperations(ctx, []store.Operation{
		{Type: store.OpInsert, Table: "small", BlockNumber: 1, LogIndex: 0,
			Data: map[string]any{"v": 1}},
	})

	// Offset beyond available events.
	events, err := s.GetEvents(ctx, "small", store.Query{Limit: 10, Offset: 100})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if events != nil {
		t.Errorf("expected nil for offset beyond results, got %d events", len(events))
	}
}

func TestDynamicContractBackfillToRoundTrip(t *testing.T) {
	s := New()
	ctx := context.Background()

	cc := &config.ContractConfig{Name: "child", Address: "0x1", Dynamic: true, Frozen: true, BackfillTo: 1234}
	if err := s.SaveDynamicContract(ctx, cc); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := s.GetDynamicContracts(ctx)
	if err != nil || len(got) != 1 {
		t.Fatalf("get: %v (%d contracts)", err, len(got))
	}
	if !got[0].Frozen || got[0].BackfillTo != 1234 {
		t.Fatalf("Frozen=%v BackfillTo=%d, want true/1234", got[0].Frozen, got[0].BackfillTo)
	}
}
