package postgres

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	pgmodule "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/b-j-roberts/ibis/internal/store"
	"github.com/b-j-roberts/ibis/internal/types"
)

func newTestStore(t *testing.T) *PostgresStore {
	t.Helper()
	ctx := context.Background()

	pgContainer, err := pgmodule.Run(ctx,
		"postgres:16-alpine",
		pgmodule.WithDatabase("ibis_test"),
		pgmodule.WithUsername("test"),
		pgmodule.WithPassword("test"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(30*time.Second),
		),
	)
	if err != nil {
		t.Fatalf("starting postgres container: %v", err)
	}

	t.Cleanup(func() {
		pgContainer.Terminate(ctx)
	})

	connStr, err := pgContainer.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("getting connection string: %v", err)
	}

	pool, err := pgxpool.New(ctx, connStr)
	if err != nil {
		t.Fatalf("connecting to postgres: %v", err)
	}
	t.Cleanup(func() { pool.Close() })

	s, err := NewFromPool(ctx, pool)
	if err != nil {
		t.Fatalf("creating postgres store: %v", err)
	}

	return s
}

func TestInsertAndGetEvents(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	err := s.CreateTable(ctx, &types.TableSchema{
		Name:      "transfers",
		TableType: types.TableTypeLog,
		Columns: []types.Column{
			{Name: "block_number", Type: "uint64"},
			{Name: "log_index", Type: "uint64"},
			{Name: "from_addr", Type: "string"},
			{Name: "to_addr", Type: "string"},
			{Name: "amount", Type: "int64"},
		},
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
				"from_addr":    "0xabc",
				"to_addr":      "0xdef",
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
				"from_addr":    "0xdef",
				"to_addr":      "0x123",
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

	s.CreateTable(ctx, &types.TableSchema{
		Name:      "events",
		TableType: types.TableTypeLog,
		Columns: []types.Column{
			{Name: "block_number", Type: "uint64"},
			{Name: "log_index", Type: "uint64"},
			{Name: "value", Type: "int64"},
		},
	})

	for i := uint64(0); i < 5; i++ {
		s.ApplyOperations(ctx, []store.Operation{{
			Type:        store.OpInsert,
			Table:       "events",
			BlockNumber: 100 + i,
			LogIndex:    0,
			Data: map[string]any{
				"block_number": 100 + i,
				"log_index":    uint64(0),
				"value":        int64(i),
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

	s.CreateTable(ctx, &types.TableSchema{
		Name:      "logs",
		TableType: types.TableTypeLog,
		Columns: []types.Column{
			{Name: "block_number", Type: "uint64"},
			{Name: "log_index", Type: "uint64"},
		},
	})

	for i := uint64(0); i < 10; i++ {
		s.ApplyOperations(ctx, []store.Operation{{
			Type:        store.OpInsert,
			Table:       "logs",
			BlockNumber: i,
			LogIndex:    0,
			Data:        map[string]any{"block_number": i, "log_index": uint64(0)},
		}})
	}

	page1, err := s.GetEvents(ctx, "logs", store.Query{Limit: 3, Offset: 0})
	if err != nil {
		t.Fatalf("get page 1: %v", err)
	}
	if len(page1) != 3 {
		t.Fatalf("expected 3 events on page 1, got %d", len(page1))
	}

	page2, err := s.GetEvents(ctx, "logs", store.Query{Limit: 3, Offset: 3})
	if err != nil {
		t.Fatalf("get page 2: %v", err)
	}
	if len(page2) != 3 {
		t.Fatalf("expected 3 events on page 2, got %d", len(page2))
	}

	if page1[2].BlockNumber == page2[0].BlockNumber {
		t.Error("page overlap detected")
	}
}

func TestFiltering(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	s.CreateTable(ctx, &types.TableSchema{
		Name:      "trades",
		TableType: types.TableTypeLog,
		Columns: []types.Column{
			{Name: "block_number", Type: "uint64"},
			{Name: "log_index", Type: "uint64"},
			{Name: "pair", Type: "string"},
			{Name: "amount", Type: "int64"},
		},
	})

	s.ApplyOperations(ctx, []store.Operation{
		{
			Type: store.OpInsert, Table: "trades", BlockNumber: 1, LogIndex: 0,
			Data: map[string]any{"pair": "ETH/USDC", "amount": 100, "block_number": uint64(1), "log_index": uint64(0)},
		},
		{
			Type: store.OpInsert, Table: "trades", BlockNumber: 2, LogIndex: 0,
			Data: map[string]any{"pair": "BTC/USDC", "amount": 200, "block_number": uint64(2), "log_index": uint64(0)},
		},
		{
			Type: store.OpInsert, Table: "trades", BlockNumber: 3, LogIndex: 0,
			Data: map[string]any{"pair": "ETH/USDC", "amount": 300, "block_number": uint64(3), "log_index": uint64(0)},
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

	s.CreateTable(ctx, &types.TableSchema{
		Name:      "events",
		TableType: types.TableTypeLog,
		Columns: []types.Column{
			{Name: "block_number", Type: "uint64"},
			{Name: "log_index", Type: "uint64"},
			{Name: "value", Type: "string"},
		},
	})

	s.ApplyOperations(ctx, []store.Operation{{
		Type: store.OpInsert, Table: "events", BlockNumber: 10, LogIndex: 0,
		Data: map[string]any{"value": "hello", "block_number": uint64(10), "log_index": uint64(0)},
	}})

	events, _ := s.GetEvents(ctx, "events", store.Query{Limit: 10})
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}

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

	s.CreateTable(ctx, &types.TableSchema{
		Name:      "events",
		TableType: types.TableTypeLog,
		Columns: []types.Column{
			{Name: "block_number", Type: "uint64"},
			{Name: "log_index", Type: "uint64"},
			{Name: "value", Type: "string"},
		},
	})

	ops := []store.Operation{
		{
			Type: store.OpInsert, Table: "events", BlockNumber: 50, LogIndex: 0,
			Data: map[string]any{"value": "a", "block_number": uint64(50), "log_index": uint64(0)},
		},
		{
			Type: store.OpInsert, Table: "events", BlockNumber: 50, LogIndex: 1,
			Data: map[string]any{"value": "b", "block_number": uint64(50), "log_index": uint64(1)},
		},
	}

	s.ApplyOperations(ctx, ops)

	events, _ := s.GetEvents(ctx, "events", store.Query{Limit: 10})
	if len(events) != 2 {
		t.Fatalf("expected 2 events before revert, got %d", len(events))
	}

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

	s.CreateTable(ctx, &types.TableSchema{
		Name:      "events",
		TableType: types.TableTypeLog,
		Columns: []types.Column{
			{Name: "block_number", Type: "uint64"},
			{Name: "log_index", Type: "uint64"},
			{Name: "value", Type: "string"},
		},
	})

	s.ApplyOperations(ctx, []store.Operation{{
		Type: store.OpInsert, Table: "events", BlockNumber: 10, LogIndex: 0,
		Data: map[string]any{"value": "original", "block_number": uint64(10), "log_index": uint64(0)},
	}})

	updateOp := store.Operation{
		Type: store.OpUpdate, Table: "events", BlockNumber: 10, LogIndex: 0,
		Data: map[string]any{"value": "updated", "block_number": uint64(10), "log_index": uint64(0)},
		Prev: map[string]any{"value": "original", "block_number": uint64(10), "log_index": uint64(0)},
	}
	s.ApplyOperations(ctx, []store.Operation{updateOp})

	events, _ := s.GetEvents(ctx, "events", store.Query{Limit: 10})
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Data["value"] != "updated" {
		t.Errorf("expected updated value, got %v", events[0].Data["value"])
	}

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
		Columns: []types.Column{
			{Name: "block_number", Type: "uint64"},
			{Name: "log_index", Type: "uint64"},
			{Name: "trader", Type: "string"},
			{Name: "score", Type: "int64"},
		},
	})

	// Two entries for same trader — unique should keep last.
	s.ApplyOperations(ctx, []store.Operation{{
		Type: store.OpInsert, Table: "leaderboard", BlockNumber: 1, LogIndex: 0,
		Data: map[string]any{"trader": "alice", "score": 100, "block_number": uint64(1), "log_index": uint64(0)},
	}})
	s.ApplyOperations(ctx, []store.Operation{{
		Type: store.OpInsert, Table: "leaderboard", BlockNumber: 2, LogIndex: 0,
		Data: map[string]any{"trader": "alice", "score": 200, "block_number": uint64(2), "log_index": uint64(0)},
	}})
	s.ApplyOperations(ctx, []store.Operation{{
		Type: store.OpInsert, Table: "leaderboard", BlockNumber: 3, LogIndex: 0,
		Data: map[string]any{"trader": "bob", "score": 150, "block_number": uint64(3), "log_index": uint64(0)},
	}})

	events, err := s.GetUniqueEvents(ctx, "leaderboard", store.Query{Limit: 10})
	if err != nil {
		t.Fatalf("get unique events: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("expected 2 unique entries, got %d", len(events))
	}

	for _, evt := range events {
		if evt.Data["trader"] == "alice" {
			if toFloat64(evt.Data["score"]) != 200 {
				t.Errorf("expected alice score 200, got %v", evt.Data["score"])
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
		Columns: []types.Column{
			{Name: "block_number", Type: "uint64"},
			{Name: "log_index", Type: "uint64"},
			{Name: "amount", Type: "int64"},
		},
		Aggregates: []types.AggregateSpec{
			{Column: "total_volume", Operation: "sum", Field: "amount"},
			{Column: "trade_count", Operation: "count"},
		},
	})

	s.ApplyOperations(ctx, []store.Operation{
		{
			Type: store.OpInsert, Table: "volume", BlockNumber: 1, LogIndex: 0,
			Data: map[string]any{"amount": 100.0, "block_number": uint64(1), "log_index": uint64(0)},
		},
		{
			Type: store.OpInsert, Table: "volume", BlockNumber: 2, LogIndex: 0,
			Data: map[string]any{"amount": 250.0, "block_number": uint64(2), "log_index": uint64(0)},
		},
		{
			Type: store.OpInsert, Table: "volume", BlockNumber: 3, LogIndex: 0,
			Data: map[string]any{"amount": 50.0, "block_number": uint64(3), "log_index": uint64(0)},
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
		Columns: []types.Column{
			{Name: "block_number", Type: "uint64"},
			{Name: "log_index", Type: "uint64"},
			{Name: "amount", Type: "int64"},
		},
		Aggregates: []types.AggregateSpec{
			{Column: "total", Operation: "sum", Field: "amount"},
			{Column: "count", Operation: "count"},
		},
	})

	ops := []store.Operation{
		{
			Type: store.OpInsert, Table: "volume", BlockNumber: 5, LogIndex: 0,
			Data: map[string]any{"amount": 100.0, "block_number": uint64(5), "log_index": uint64(0)},
		},
		{
			Type: store.OpInsert, Table: "volume", BlockNumber: 5, LogIndex: 1,
			Data: map[string]any{"amount": 200.0, "block_number": uint64(5), "log_index": uint64(1)},
		},
	}

	s.ApplyOperations(ctx, ops)

	result, _ := s.GetAggregation(ctx, "volume", store.Query{})
	if toFloat64(result.Values["total"]) != 300.0 {
		t.Fatalf("expected total 300 before revert, got %v", result.Values["total"])
	}

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

	cursor, err := s.GetCursor(ctx, "mycontract")
	if err != nil {
		t.Fatalf("get cursor: %v", err)
	}
	if cursor != 0 {
		t.Errorf("expected initial cursor 0, got %d", cursor)
	}

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

	s.SetCursor(ctx, "mycontract", 99999)
	cursor, _ = s.GetCursor(ctx, "mycontract")
	if cursor != 99999 {
		t.Errorf("expected cursor 99999, got %d", cursor)
	}
}

func TestCreateAndMigrateTable(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	sch := types.TableSchema{
		Name:      "test_table",
		Contract:  "TestContract",
		Event:     "Transfer",
		TableType: types.TableTypeLog,
		Columns: []types.Column{
			{Name: "block_number", Type: "uint64"},
			{Name: "log_index", Type: "uint64"},
			{Name: "from_addr", Type: "string"},
			{Name: "to_addr", Type: "string"},
		},
	}

	if err := s.CreateTable(ctx, &sch); err != nil {
		t.Fatalf("create table: %v", err)
	}

	stored, ok := s.schemas["test_table"]
	if !ok {
		t.Fatal("schema not found after create")
	}
	if stored.Event != "Transfer" {
		t.Errorf("expected event Transfer, got %s", stored.Event)
	}

	// Migrate: add a column.
	sch.Columns = append(sch.Columns, types.Column{Name: "amount", Type: "int64"})
	if err := s.MigrateTable(ctx, &sch); err != nil {
		t.Fatalf("migrate table: %v", err)
	}

	stored = s.schemas["test_table"]
	if len(stored.Columns) != 5 {
		t.Errorf("expected 5 columns after migration, got %d", len(stored.Columns))
	}

	// Verify new column works by inserting data with it.
	err := s.ApplyOperations(ctx, []store.Operation{{
		Type: store.OpInsert, Table: "test_table", BlockNumber: 1, LogIndex: 0,
		Data: map[string]any{
			"block_number": uint64(1), "log_index": uint64(0),
			"from_addr": "0x1", "to_addr": "0x2", "amount": int64(1000),
		},
	}})
	if err != nil {
		t.Fatalf("insert after migration: %v", err)
	}

	events, _ := s.GetEvents(ctx, "test_table", store.Query{Limit: 10})
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
}

// TestCreateTableFailureDoesNotCacheSchema guards the ordering CreateTable
// relies on: storeSchema is only called after the DDL has actually
// succeeded. A table name with an unquoted hyphen breaks CREATE TABLE's own
// generated SQL (generateCreateTableDDL interpolates sch.Name unquoted --
// Postgres parses the hyphen as subtraction), guaranteeing a genuine DDL
// failure distinct from the concurrent-create race isConcurrentCreateRace
// tolerates. If a schema were cached before the DDL succeeds, anything
// built on top of CreateTable that skips re-issuing DDL for an
// already-known table name (see the dedup PR stacked on this one) would
// silently believe a table exists when it was never actually created.
func TestCreateTableFailureDoesNotCacheSchema(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	broken := types.TableSchema{
		Name:      "bad-table-name",
		TableType: types.TableTypeLog,
		Columns:   []types.Column{{Name: "block_number", Type: "uint64"}},
	}
	if err := s.CreateTable(ctx, &broken); err == nil {
		t.Fatal("expected CreateTable to fail for a table name that breaks the generated DDL")
	}

	if _, exists := s.lookupSchema("bad-table-name"); exists {
		t.Fatal("a failed CreateTable must not cache the schema for a table that was never created")
	}
}

// TestCreateTableSkipsRedundantDDL covers the shared/factory-table case:
// Engine.setup() reloads every persisted dynamic contract on cold start and
// calls CreateTable per contract with no dedup cache across contracts of the
// same factory, so CreateTable can be called more than once with the same
// schema.Name. A second call whose columns (and their types) are already
// all present must be a cheap no-op rather than re-issuing DDL -- with
// hundreds of persisted children sharing a handful of factory tables,
// re-running it every time turns into hundreds of redundant Postgres round
// trips on a cold start.
func TestCreateTableSkipsRedundantDDL(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	sch := types.TableSchema{
		Name:      "shared_table",
		Contract:  "Factory",
		Event:     "Transfer",
		TableType: types.TableTypeLog,
		Columns: []types.Column{
			{Name: "block_number", Type: "uint64"},
			{Name: "log_index", Type: "uint64"},
			{Name: "from_addr", Type: "string"},
		},
	}
	if err := s.CreateTable(ctx, &sch); err != nil {
		t.Fatalf("create table: %v", err)
	}

	// A second dynamic child registering the exact same columns hits the
	// name+columns cache hit and never reaches reconcileSchema.
	dup := sch
	if err := s.CreateTable(ctx, &dup); err != nil {
		t.Fatalf("second CreateTable for same table name: %v", err)
	}

	stored, ok := s.lookupSchema("shared_table")
	if !ok {
		t.Fatal("schema missing after second CreateTable")
	}
	if len(stored.Columns) != 3 {
		t.Errorf("expected the cached 3-column schema to be unchanged, got %d columns", len(stored.Columns))
	}

	if err := s.ApplyOperations(ctx, []store.Operation{{
		Type: store.OpInsert, Table: "shared_table", BlockNumber: 1, LogIndex: 0,
		Data: map[string]any{
			"block_number": uint64(1), "log_index": uint64(0), "from_addr": "0x1",
		},
	}}); err != nil {
		t.Fatalf("insert into shared table: %v", err)
	}
}

// TestCreateTableReconcilesNewColumns covers schema drift across dynamic
// children sharing a factory table: a second child whose ABI has an extra
// field must widen the live table via reconcileSchema, rather than being
// silently skipped or rejected. The end state must include both the
// original and the new column, and both must be usable.
func TestCreateTableReconcilesNewColumns(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	sch := types.TableSchema{
		Name:      "reconciled_table",
		Contract:  "Factory",
		Event:     "TransferA",
		TableType: types.TableTypeLog,
		Columns: []types.Column{
			{Name: "block_number", Type: "uint64"},
			{Name: "log_index", Type: "uint64"},
			{Name: "from_addr", Type: "string"},
		},
	}
	if err := s.CreateTable(ctx, &sch); err != nil {
		t.Fatalf("create table: %v", err)
	}

	// A second child with an extra column must widen the live table.
	widened := sch
	widened.Event = "TransferB"
	widened.Columns = append(widened.Columns, types.Column{Name: "extra", Type: "string"})
	if err := s.CreateTable(ctx, &widened); err != nil {
		t.Fatalf("reconciling CreateTable with a new column: %v", err)
	}

	stored, ok := s.lookupSchema("reconciled_table")
	if !ok {
		t.Fatal("schema missing after reconciling CreateTable")
	}
	if len(stored.Columns) != 4 {
		t.Fatalf("expected the reconciled schema to have 4 columns, got %d", len(stored.Columns))
	}

	if err := s.ApplyOperations(ctx, []store.Operation{
		{
			Type: store.OpInsert, Table: "reconciled_table", BlockNumber: 1, LogIndex: 0,
			Data: map[string]any{"block_number": uint64(1), "log_index": uint64(0), "from_addr": "0x1"},
		},
		{
			Type: store.OpInsert, Table: "reconciled_table", BlockNumber: 2, LogIndex: 0,
			Data: map[string]any{"block_number": uint64(2), "log_index": uint64(0), "from_addr": "0x2", "extra": "hi"},
		},
	}); err != nil {
		t.Fatalf("insert using pre- and post-reconciliation columns: %v", err)
	}

	events, err := s.GetEvents(ctx, "reconciled_table", store.Query{Limit: 10})
	if err != nil {
		t.Fatalf("get events: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(events))
	}
}

// TestSharedTableSchemaOrderIndependence proves reconciliation is
// commutative: three schemas (X, Y, Z) for a single shared table, each with
// a unique column, a column shared with exactly one other schema, and a
// column shared with both other schemas, must converge on the exact same
// table shape and contents regardless of which order the three dynamic
// children happen to register in.
func TestSharedTableSchemaOrderIndependence(t *testing.T) {
	// X: unique_x, shared_xy, shared_xz, shared_all
	// Y: unique_y, shared_xy, shared_yz, shared_all
	// Z: unique_z, shared_xz, shared_yz, shared_all
	base := func(name string, extra ...string) types.TableSchema {
		cols := []types.Column{
			{Name: "block_number", Type: "uint64"},
			{Name: "log_index", Type: "uint64"},
		}
		for _, c := range extra {
			cols = append(cols, types.Column{Name: c, Type: "string"})
		}
		return types.TableSchema{
			Name:      "shared_xyz",
			Contract:  "Factory",
			Event:     name,
			TableType: types.TableTypeLog,
			Columns:   cols,
		}
	}
	schemas := map[string]types.TableSchema{
		"X": base("X", "unique_x", "shared_xy", "shared_xz", "shared_all"),
		"Y": base("Y", "unique_y", "shared_xy", "shared_yz", "shared_all"),
		"Z": base("Z", "unique_z", "shared_xz", "shared_yz", "shared_all"),
	}
	blockFor := map[string]uint64{"X": 1, "Y": 2, "Z": 3}

	wantCols := map[string]bool{
		"block_number": true, "log_index": true,
		"unique_x": true, "unique_y": true, "unique_z": true,
		"shared_xy": true, "shared_xz": true, "shared_yz": true,
		"shared_all": true,
	}

	orderings := [][]string{
		{"X", "Y", "Z"}, {"X", "Z", "Y"},
		{"Y", "X", "Z"}, {"Y", "Z", "X"},
		{"Z", "X", "Y"}, {"Z", "Y", "X"},
	}

	for _, order := range orderings {
		t.Run(fmt.Sprintf("%v", order), func(t *testing.T) {
			s := newTestStore(t)
			ctx := context.Background()

			for _, name := range order {
				sch := schemas[name]
				if err := s.CreateTable(ctx, &sch); err != nil {
					t.Fatalf("CreateTable(%s) in order %v: %v", name, order, err)
				}
			}

			stored, ok := s.lookupSchema("shared_xyz")
			if !ok {
				t.Fatal("schema missing after registering all three")
			}
			gotCols := make(map[string]bool, len(stored.Columns))
			for _, c := range stored.Columns {
				gotCols[c.Name] = true
			}
			if len(gotCols) != len(wantCols) {
				t.Fatalf("order %v: expected %d columns, got %d (%v)", order, len(wantCols), len(gotCols), gotCols)
			}
			for name := range wantCols {
				if !gotCols[name] {
					t.Errorf("order %v: missing expected column %q", order, name)
				}
			}

			// Insert one row per schema, using only that schema's own columns.
			var ops []store.Operation
			for _, name := range order {
				sch := schemas[name]
				data := map[string]any{
					"block_number": blockFor[name], "log_index": uint64(0),
				}
				for _, c := range sch.Columns {
					if c.Name == "block_number" || c.Name == "log_index" {
						continue
					}
					data[c.Name] = name
				}
				ops = append(ops, store.Operation{
					Type: store.OpInsert, Table: "shared_xyz",
					BlockNumber: blockFor[name], LogIndex: 0, Data: data,
				})
			}
			if err := s.ApplyOperations(ctx, ops); err != nil {
				t.Fatalf("order %v: inserting rows: %v", order, err)
			}

			events, err := s.GetEvents(ctx, "shared_xyz", store.Query{Limit: 10})
			if err != nil {
				t.Fatalf("order %v: get events: %v", order, err)
			}
			if len(events) != 3 {
				t.Fatalf("order %v: expected 3 rows, got %d", order, len(events))
			}
			for _, evt := range events {
				name, ok := map[uint64]string{1: "X", 2: "Y", 3: "Z"}[evt.BlockNumber]
				if !ok {
					t.Fatalf("order %v: unexpected block_number %d", order, evt.BlockNumber)
				}
				sch := schemas[name]
				for _, c := range sch.Columns {
					if c.Name == "block_number" || c.Name == "log_index" {
						continue
					}
					if got := evt.Data[c.Name]; got != name {
						t.Errorf("order %v: row for %s: column %s = %v, want %s", order, name, c.Name, got, name)
					}
				}
			}
		})
	}
}

func TestEmptyTableReturnsNoEvents(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	s.CreateTable(ctx, &types.TableSchema{
		Name:      "empty",
		TableType: types.TableTypeLog,
		Columns: []types.Column{
			{Name: "block_number", Type: "uint64"},
			{Name: "log_index", Type: "uint64"},
		},
	})

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

	s.CreateTable(ctx, &types.TableSchema{
		Name:      "events",
		TableType: types.TableTypeLog,
		Columns: []types.Column{
			{Name: "block_number", Type: "uint64"},
			{Name: "log_index", Type: "uint64"},
			{Name: "value", Type: "string"},
		},
	})

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

	if events[0].LogIndex != 0 || events[1].LogIndex != 1 || events[2].LogIndex != 2 {
		t.Error("events not ordered by log index within block")
	}
}

func TestFilterOperators(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	s.CreateTable(ctx, &types.TableSchema{
		Name:      "data",
		TableType: types.TableTypeLog,
		Columns: []types.Column{
			{Name: "block_number", Type: "uint64"},
			{Name: "log_index", Type: "uint64"},
			{Name: "score", Type: "int64"},
		},
	})

	for i := 0; i < 5; i++ {
		s.ApplyOperations(ctx, []store.Operation{{
			Type: store.OpInsert, Table: "data", BlockNumber: uint64(i), LogIndex: 0,
			Data: map[string]any{"score": i * 10, "block_number": uint64(i), "log_index": uint64(0)},
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

func TestAggregationAvg(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	s.CreateTable(ctx, &types.TableSchema{
		Name:      "stats",
		TableType: types.TableTypeAggregation,
		Columns: []types.Column{
			{Name: "block_number", Type: "uint64"},
			{Name: "log_index", Type: "uint64"},
			{Name: "score", Type: "int64"},
		},
		Aggregates: []types.AggregateSpec{
			{Column: "avg_score", Operation: "avg", Field: "score"},
		},
	})

	s.ApplyOperations(ctx, []store.Operation{
		{Type: store.OpInsert, Table: "stats", BlockNumber: 1, LogIndex: 0,
			Data: map[string]any{"score": 10.0, "block_number": uint64(1), "log_index": uint64(0)}},
		{Type: store.OpInsert, Table: "stats", BlockNumber: 2, LogIndex: 0,
			Data: map[string]any{"score": 20.0, "block_number": uint64(2), "log_index": uint64(0)}},
		{Type: store.OpInsert, Table: "stats", BlockNumber: 3, LogIndex: 0,
			Data: map[string]any{"score": 30.0, "block_number": uint64(3), "log_index": uint64(0)}},
	})

	result, _ := s.GetAggregation(ctx, "stats", store.Query{})
	avg := toFloat64(result.Values["avg_score"])
	if avg != 20.0 {
		t.Errorf("expected avg 20, got %v", avg)
	}
}

// TestConcurrentSchemaAccessIsRaceFree exercises exactly the pattern dynamic
// contract registration produces in practice: CreateTable calls for a
// handful of shared table names running concurrently with each other and
// with the query/insert paths that read the same schemas -- registration
// deliberately happens outside the engine's own lock (see
// factory.go/discover.go's "I/O outside the lock" sections), so nothing
// serializes access to the store itself. Before schemasMu, this was an
// unguarded map hit from multiple goroutines: a data race at best, and a
// fatal, unrecoverable `fatal error: concurrent map read and map write`
// crash at worst, not just a slow test. Run with `go test -race` to verify.
func TestConcurrentSchemaAccessIsRaceFree(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	const tableCount = 5
	const workersPerTable = 8

	schemaFor := func(i int) types.TableSchema {
		return types.TableSchema{
			Name:      fmt.Sprintf("concurrent_table_%d", i),
			Contract:  "Factory",
			Event:     "Transfer",
			TableType: types.TableTypeLog,
			Columns: []types.Column{
				{Name: "block_number", Type: "uint64"},
				{Name: "log_index", Type: "uint64"},
				{Name: "from_addr", Type: "string"},
			},
		}
	}

	var wg sync.WaitGroup
	errs := make(chan error, tableCount*workersPerTable*3)

	for i := 0; i < tableCount; i++ {
		sch := schemaFor(i)
		for w := 0; w < workersPerTable; w++ {
			wg.Add(1)
			go func(sch types.TableSchema, block uint64) {
				defer wg.Done()
				// Every worker races to create the same table name --
				// idempotent DDL, but the in-memory cache write is where an
				// unguarded map would crash or race.
				if err := s.CreateTable(ctx, &sch); err != nil {
					errs <- fmt.Errorf("CreateTable(%s): %w", sch.Name, err)
					return
				}
				// Concurrently insert and read back -- exercises every other
				// lookupSchema call site (insertRow, buildSelectQuery,
				// scanEvents, hasLogIndexColumn) while registration is still
				// happening on other goroutines.
				if err := s.ApplyOperations(ctx, []store.Operation{{
					Type: store.OpInsert, Table: sch.Name, BlockNumber: block, LogIndex: 0,
					Data: map[string]any{
						"block_number": block, "log_index": uint64(0), "from_addr": "0x1",
					},
				}}); err != nil {
					errs <- fmt.Errorf("insert into %s: %w", sch.Name, err)
					return
				}
				if _, err := s.GetEvents(ctx, sch.Name, store.Query{Limit: 1}); err != nil {
					errs <- fmt.Errorf("get events for %s: %w", sch.Name, err)
				}
			}(sch, uint64(w+1))
		}
	}

	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

// TestCreateTableDoesNotSwallowRealUniqueViolation guards
// isConcurrentCreateRace's ConstraintName check: generateCreateTableDDL
// batches CREATE TABLE with its CREATE UNIQUE INDEX statement into one Exec
// call, so a 23505 from real duplicate data (not a concurrent-create race)
// must still surface as an error, not be swallowed as if it were benign.
//
// The dirty data is seeded via a raw SQL statement rather than a first
// CreateTable call + inserts, deliberately: a second CreateTable call for an
// already-cached table name can be short-circuited by dedup logic built on
// top of this package (see the PR stacked on this one), which would never
// reach the DDL this test needs to exercise. Seeding out-of-band keeps this
// test about isConcurrentCreateRace specifically, independent of whatever
// fast-path skip CreateTable itself grows later.
func TestCreateTableDoesNotSwallowRealUniqueViolation(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// Seed a table with two rows sharing the same "trader" value -- no
	// unique constraint exists yet, so nothing prevents this.
	if _, err := s.pool.Exec(ctx, `
		CREATE TABLE dirty_leaderboard (block_number BIGINT, log_index BIGINT, trader TEXT);
		INSERT INTO dirty_leaderboard (block_number, log_index, trader) VALUES (1, 0, 'alice'), (2, 0, 'alice');
	`); err != nil {
		t.Fatalf("seeding duplicate trader rows: %v", err)
	}

	// Registering the same table name as unique on "trader" now must fail --
	// CREATE UNIQUE INDEX cannot succeed against the duplicate data already
	// present, and that failure must not be mistaken for the benign
	// concurrent-create race.
	err := s.CreateTable(ctx, &types.TableSchema{
		Name:      "dirty_leaderboard",
		TableType: types.TableTypeUnique,
		UniqueKey: "trader",
		Columns: []types.Column{
			{Name: "block_number", Type: "uint64"},
			{Name: "log_index", Type: "uint64"},
			{Name: "trader", Type: "string"},
		},
	})
	if err == nil {
		t.Fatal("expected CreateTable to fail when a unique index collides with real duplicate data, got nil")
	}
}
