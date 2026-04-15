package badger

import (
	"context"
	"testing"

	"github.com/b-j-roberts/ibis/internal/store"
	"github.com/b-j-roberts/ibis/internal/types"
)

func newTestStore(t *testing.T) *BadgerStore {
	t.Helper()
	s, err := NewInMemory()
	if err != nil {
		t.Fatalf("creating in-memory badger store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestInsertAndGetEvents(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	err := s.CreateTable(ctx, &types.TableSchema{
		Name:      "transfers",
		TableType: types.TableTypeLog,
	})
	if err != nil {
		t.Fatalf("create table: %v", err)
	}

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

func TestCursorPersistence(t *testing.T) {
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

func TestInverseOp(t *testing.T) {
	insert := store.Operation{
		Type: store.OpInsert, Table: "t", Key: "k",
		Data: map[string]any{"v": 1}, BlockNumber: 10,
	}
	inv := insert.InverseOp()
	if inv.Type != store.OpDelete {
		t.Errorf("expected inverse of insert to be delete, got %v", inv.Type)
	}

	update := store.Operation{
		Type: store.OpUpdate, Table: "t", Key: "k",
		Data: map[string]any{"v": 2}, Prev: map[string]any{"v": 1}, BlockNumber: 10,
	}
	inv = update.InverseOp()
	if inv.Type != store.OpUpdate {
		t.Errorf("expected inverse of update to be update, got %v", inv.Type)
	}
	if inv.Data["v"] != 1 {
		t.Errorf("expected inverse update data to be prev, got %v", inv.Data["v"])
	}

	del := store.Operation{
		Type: store.OpDelete, Table: "t", Key: "k",
		Prev: map[string]any{"v": 1}, BlockNumber: 10,
	}
	inv = del.InverseOp()
	if inv.Type != store.OpInsert {
		t.Errorf("expected inverse of delete to be insert, got %v", inv.Type)
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

func TestSchemasPersistAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	// Create store and add a schema.
	s1, err := New(dir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	s1.CreateTable(ctx, &types.TableSchema{
		Name:      "persisted",
		Contract:  "C1",
		Event:     "E1",
		TableType: types.TableTypeLog,
	})
	s1.Close()

	// Reopen and verify schema is loaded.
	s2, err := New(dir)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer s2.Close()

	s2.mu.RLock()
	schema, ok := s2.schemas["persisted"]
	s2.mu.RUnlock()
	if !ok {
		t.Fatal("schema not persisted across reopen")
	}
	if schema.Event != "E1" {
		t.Errorf("expected event E1, got %s", schema.Event)
	}
}

// TestFilterOperatorsWithStringValues exercises range filters where the filter
// value is a string, which is the real-world path: API query params like
// ?score=gt.20 are parsed into store.Filter{Value: "20"} (a string).
// The original TestFilterOperators uses raw int values, masking the bug.
func TestFilterOperatorsWithStringValues(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	s.CreateTable(ctx, &types.TableSchema{Name: "data", TableType: types.TableTypeLog})

	// Insert events with int scores (simulating what the indexer stores).
	for i := 0; i < 5; i++ {
		s.ApplyOperations(ctx, []store.Operation{{
			Type: store.OpInsert, Table: "data", BlockNumber: uint64(i), LogIndex: 0,
			Data: map[string]any{"score": i * 10, "block_number": uint64(i)},
		}})
	}

	// Filter values are strings, simulating the API query param path.
	tests := []struct {
		name     string
		filter   store.Filter
		expected int
	}{
		{"gt_string", store.Filter{Field: "score", Operator: "gt", Value: "20"}, 2},
		{"gte_string", store.Filter{Field: "score", Operator: "gte", Value: "20"}, 3},
		{"lt_string", store.Filter{Field: "score", Operator: "lt", Value: "20"}, 2},
		{"lte_string", store.Filter{Field: "score", Operator: "lte", Value: "20"}, 3},
		{"eq_string", store.Filter{Field: "score", Operator: "eq", Value: "20"}, 1},
		{"neq_string", store.Filter{Field: "score", Operator: "neq", Value: "20"}, 4},
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

// TestSortEventsWithStringNumerics verifies that sortEvents works correctly
// when data fields are string-typed numerics (e.g., after JSON round-tripping).
func TestSortEventsWithStringNumerics(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	s.CreateTable(ctx, &types.TableSchema{
		Name:      "scores",
		TableType: types.TableTypeUnique,
		UniqueKey: "player",
	})

	// Insert events with string-typed score values (simulating JSON round-trip).
	players := []struct {
		name  string
		score string
		block uint64
	}{
		{"alice", "300", 1},
		{"bob", "100", 2},
		{"charlie", "200", 3},
	}

	for _, p := range players {
		s.ApplyOperations(ctx, []store.Operation{{
			Type: store.OpInsert, Table: "scores", BlockNumber: p.block, LogIndex: 0,
			Data: map[string]any{
				"player":       p.name,
				"score":        p.score,
				"block_number": p.block,
			},
		}})
	}

	// Sort ascending by score.
	events, err := s.GetUniqueEvents(ctx, "scores", store.Query{
		Limit:    10,
		OrderBy:  "score",
		OrderDir: store.OrderAsc,
	})
	if err != nil {
		t.Fatalf("get unique events: %v", err)
	}
	if len(events) != 3 {
		t.Fatalf("expected 3 events, got %d", len(events))
	}

	// Ascending: 100, 200, 300.
	if events[0].Data["player"] != "bob" {
		t.Errorf("expected first player bob (score 100), got %v", events[0].Data["player"])
	}
	if events[1].Data["player"] != "charlie" {
		t.Errorf("expected second player charlie (score 200), got %v", events[1].Data["player"])
	}
	if events[2].Data["player"] != "alice" {
		t.Errorf("expected third player alice (score 300), got %v", events[2].Data["player"])
	}

	// Sort descending by score.
	events, err = s.GetUniqueEvents(ctx, "scores", store.Query{
		Limit:    10,
		OrderBy:  "score",
		OrderDir: store.OrderDesc,
	})
	if err != nil {
		t.Fatalf("get unique events desc: %v", err)
	}

	// Descending: 300, 200, 100.
	if events[0].Data["player"] != "alice" {
		t.Errorf("expected first player alice (score 300), got %v", events[0].Data["player"])
	}
	if events[2].Data["player"] != "bob" {
		t.Errorf("expected last player bob (score 100), got %v", events[2].Data["player"])
	}
}

// TestSSEReplayFilterSimulation verifies that the SSE replay filter path works
// correctly. SSE replay uses fmt.Sprintf("%d", block) which produces a string
// value for the block_number gte filter.
func TestSSEReplayFilterSimulation(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	s.CreateTable(ctx, &types.TableSchema{Name: "events", TableType: types.TableTypeLog})

	for i := uint64(850000); i < 850005; i++ {
		s.ApplyOperations(ctx, []store.Operation{{
			Type: store.OpInsert, Table: "events", BlockNumber: i, LogIndex: 0,
			Data: map[string]any{"block_number": i, "value": "test"},
		}})
	}

	// Simulate SSE replay filter: Value is fmt.Sprintf("%d", 850002) = "850002"
	events, err := s.GetEvents(ctx, "events", store.Query{
		Limit: 10,
		Filters: []store.Filter{{
			Field:    "block_number",
			Operator: "gte",
			Value:    "850002",
		}},
	})
	if err != nil {
		t.Fatalf("get filtered events: %v", err)
	}
	// Blocks 850002, 850003, 850004 = 3 events.
	if len(events) != 3 {
		t.Errorf("expected 3 events with block_number >= 850002, got %d", len(events))
	}
}

func TestCursorPersistsAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	s1, _ := New(dir)
	s1.SetCursor(ctx, "mycontract", 42)
	s1.Close()

	s2, _ := New(dir)
	defer s2.Close()

	cursor, _ := s2.GetCursor(ctx, "mycontract")
	if cursor != 42 {
		t.Errorf("expected cursor 42 after reopen, got %d", cursor)
	}
}
