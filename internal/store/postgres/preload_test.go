package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	pgmodule "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/b-j-roberts/ibis/internal/types"
)

// ---- Pure unit tests: decision logic, no database required ----

func TestLiveColumnsCoverSchema(t *testing.T) {
	// Realistic shape of a table generateCreateTableDDL actually created:
	// block_number/log_index are the only columns ever given a NOT NULL
	// clause (Nullable: false); every other column -- "trader" here -- is
	// always created nullable regardless of what its own schema said.
	live := []types.Column{
		{Name: "block_number", Type: "int64", Nullable: false},
		{Name: "log_index", Type: "int64", Nullable: false},
		{Name: "trader", Type: "string", Nullable: true},
	}

	t.Run("exact match covers", func(t *testing.T) {
		incoming := &types.TableSchema{Columns: []types.Column{
			{Name: "block_number", Type: "int64"},
			{Name: "trader", Type: "string"},
		}}
		if !liveColumnsCoverSchema(live, incoming) {
			t.Fatal("expected live columns to cover an exact-type subset")
		}
	})

	t.Run("uint64 vs int64 both map to BIGINT, still covers", func(t *testing.T) {
		// information_schema only round-trips "bigint", which
		// rawPostgresTypeToColumnType always resolves to our "int64" label
		// -- a schema declaring the same column as "uint64" (extremely
		// common for ABI fields like block_number) must still be treated
		// as covered, since both types map to the same Postgres BIGINT
		// column via columnTypeToPostgres.
		incoming := &types.TableSchema{Columns: []types.Column{
			{Name: "block_number", Type: "uint64"},
			{Name: "log_index", Type: "uint64"},
		}}
		if !liveColumnsCoverSchema(live, incoming) {
			t.Fatal("expected uint64 column to be covered by an int64 (BIGINT) live column")
		}
	})

	t.Run("missing column does not cover", func(t *testing.T) {
		incoming := &types.TableSchema{Columns: []types.Column{
			{Name: "block_number", Type: "int64"},
			{Name: "new_field", Type: "string"},
		}}
		if liveColumnsCoverSchema(live, incoming) {
			t.Fatal("expected a column absent from live to be reported as not covered")
		}
	})

	t.Run("incompatible type does not cover", func(t *testing.T) {
		incoming := &types.TableSchema{Columns: []types.Column{
			{Name: "trader", Type: "int64"}, // live has "string" (TEXT)
		}}
		if liveColumnsCoverSchema(live, incoming) {
			t.Fatal("expected a genuinely incompatible column type to be reported as not covered")
		}
	})

	t.Run("empty incoming columns trivially covers", func(t *testing.T) {
		incoming := &types.TableSchema{Columns: nil}
		if !liveColumnsCoverSchema(live, incoming) {
			t.Fatal("expected no columns requested to trivially be covered")
		}
	})

	t.Run("nullability drift on block_number does not cover", func(t *testing.T) {
		// Mirrors TestCreateTableDoesNotSwallowRealUniqueViolation: a table
		// seeded out-of-band (or by an older code path) without a NOT NULL
		// on block_number. reconcileSchema's Atlas diff would flag this as
		// an incompatible-schema ModifyColumn and refuse to proceed -- the
		// preload fast path must not paper over that by treating a merely
		// type-compatible column as fully covered.
		driftedLive := []types.Column{
			{Name: "block_number", Type: "int64", Nullable: true}, // should be false
			{Name: "log_index", Type: "int64", Nullable: false},
			{Name: "trader", Type: "string", Nullable: true},
		}
		incoming := &types.TableSchema{Columns: []types.Column{
			{Name: "block_number", Type: "uint64"}, // Nullable: false (default) -> wants NOT NULL
			{Name: "log_index", Type: "uint64"},
			{Name: "trader", Type: "string"},
		}}
		if liveColumnsCoverSchema(driftedLive, incoming) {
			t.Fatal("expected a nullability-drifted block_number column to be reported as not covered, so it falls through to reconcileSchema's stricter check")
		}
	})
}

func TestMergeLiveColumns(t *testing.T) {
	live := []types.Column{
		{Name: "block_number", Type: "int64"},
		{Name: "trader", Type: "string"},
		{Name: "extra_from_another_child", Type: "string", Nullable: true},
	}
	sch := &types.TableSchema{
		Name:      "orders",
		TableType: types.TableTypeLog,
		Columns: []types.Column{
			{Name: "block_number", Type: "uint64"},
			{Name: "trader", Type: "string"},
		},
	}

	merged := mergeLiveColumns(live, sch)

	if merged.Name != sch.Name || merged.TableType != sch.TableType {
		t.Fatalf("expected merge to preserve schema metadata, got %+v", merged)
	}
	if len(merged.Columns) != 3 {
		t.Fatalf("expected 3 columns (2 own + 1 live-only), got %d: %+v", len(merged.Columns), merged.Columns)
	}

	byName := make(map[string]types.Column, len(merged.Columns))
	for _, c := range merged.Columns {
		byName[c.Name] = c
	}

	// sch's own columns keep their own declared type label, not the
	// coarser one inferred from information_schema.
	if got := byName["block_number"].Type; got != "uint64" {
		t.Errorf("expected sch's own column to keep its declared type \"uint64\", got %q", got)
	}
	extra, ok := byName["extra_from_another_child"]
	if !ok {
		t.Fatal("expected the live-only column to be merged in")
	}
	if extra.Type != "string" || !extra.Nullable {
		t.Errorf("expected the live-only column's own type/nullability to be preserved, got %+v", extra)
	}
}

// TestCreateTableReport_PreloadFastPath exercises CreateTableReport's
// preload branch without a database: liveColumns is populated directly and
// livePreloadOnce is pre-triggered as a no-op, so preloadLiveColumns's real
// query body never runs and s.pool is never touched.
func TestCreateTableReport_PreloadFastPath(t *testing.T) {
	s := &PostgresStore{
		schemas: make(map[string]types.TableSchema),
		liveColumns: map[string][]types.Column{
			// Realistic shape: only block_number/log_index are NOT NULL;
			// every other column is nullable, matching generateCreateTableDDL.
			"orders": {
				{Name: "block_number", Type: "int64", Nullable: false},
				{Name: "log_index", Type: "int64", Nullable: false},
				{Name: "trader", Type: "string", Nullable: true},
			},
		},
	}
	s.livePreloadOnce.Do(func() {}) // pretend the bulk preload already ran

	sch := &types.TableSchema{
		Name:      "orders",
		TableType: types.TableTypeLog, // not aggregation -- ensureAggTable is a no-op, no s.pool access
		Columns: []types.Column{
			{Name: "block_number", Type: "uint64"},
			{Name: "log_index", Type: "uint64"},
			{Name: "trader", Type: "string"},
		},
	}

	changed, err := s.CreateTableReport(context.Background(), sch)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if changed {
		t.Fatal("expected changed=false: the live map already covers the schema, no round trip should have been needed")
	}

	cached, ok := s.lookupSchema("orders")
	if !ok {
		t.Fatal("expected the schema to be cached after a preload fast-path hit")
	}
	if len(cached.Columns) != 3 {
		t.Fatalf("expected cached schema to retain all 3 columns, got %d", len(cached.Columns))
	}

	// A second call for the same table now hits the warm s.schemas cache
	// (hasAllColumns), the pre-existing fast path -- also changed=false,
	// and still no s.pool access.
	changed, err = s.CreateTableReport(context.Background(), sch)
	if err != nil {
		t.Fatalf("unexpected error on second call: %v", err)
	}
	if changed {
		t.Fatal("expected changed=false on a warm in-process cache hit")
	}
}

// TestCreateTableReport_PreloadMiss_FallsThroughWithoutPanicking documents
// that a table absent from the live preload map (e.g. never created by any
// previous run) is correctly reported as "not covered" by
// liveColumnsCoverSchema/the map lookup itself, so CreateTableReport's
// preload branch is skipped and control reaches the pre-existing
// reconcileSchema call -- exercised end-to-end (this table doesn't actually
// exist in the pool it points calls to reconcileSchema which needs a real
// atlas/pool) in the testcontainers-backed tests in postgres_test.go.
// This test only pins down the map-miss part of the decision.
func TestCreateTableReport_PreloadMiss(t *testing.T) {
	s := &PostgresStore{
		liveColumns: map[string][]types.Column{
			"unrelated_table": {{Name: "x", Type: "string"}},
		},
	}
	if _, ok := s.liveColumns["never_created"]; ok {
		t.Fatal("test setup bug: table should not be present in the live map")
	}
}

// ---- Integration tests: real Postgres via testcontainers, following the
// same newTestStore pattern as postgres_test.go. Like every other test in
// this package, these need a running Docker daemon; there is no DSN-based
// opt-out in this codebase's existing test style.

func newTestPostgresConnString(t *testing.T) string {
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
	t.Cleanup(func() { pgContainer.Terminate(ctx) })

	connStr, err := pgContainer.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("getting connection string: %v", err)
	}
	return connStr
}

func newStoreFromConnString(t *testing.T, connStr string) *PostgresStore {
	t.Helper()
	ctx := context.Background()

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

// TestCreateTableReport_PreloadAcrossColdBoot simulates a real cold boot:
// storeA creates a table (standing in for a first process/run). storeB then
// points at the exact same database with its own empty in-process cache --
// standing in for a second process, e.g. after a restart, or the scenario
// this whole change targets: a single process re-registering ~23.4k
// persisted dynamic children against ~549 already-existing tables.
// Registering the same schema against storeB must resolve via the bulk
// preload: changed=false, no per-table Atlas round trip.
func TestCreateTableReport_PreloadAcrossColdBoot(t *testing.T) {
	connStr := newTestPostgresConnString(t)
	ctx := context.Background()

	storeA := newStoreFromConnString(t, connStr)
	sch := &types.TableSchema{
		Name:      "orders",
		TableType: types.TableTypeLog,
		Columns: []types.Column{
			{Name: "block_number", Type: "uint64"},
			{Name: "log_index", Type: "uint64"},
			{Name: "trader", Type: "string"},
		},
	}
	if changed, err := storeA.CreateTableReport(ctx, sch); err != nil {
		t.Fatalf("storeA create table: %v", err)
	} else if !changed {
		t.Fatal("expected the very first creation of a brand-new table to report changed=true")
	}

	storeB := newStoreFromConnString(t, connStr)
	changed, err := storeB.CreateTableReport(ctx, sch)
	if err != nil {
		t.Fatalf("storeB create table: %v", err)
	}
	if changed {
		t.Fatal("expected storeB's first call for an already-existing, fully-covered table to report changed=false (preload hit, no round trip)")
	}
	if len(storeB.liveColumns) == 0 {
		t.Fatal("expected the bulk preload to have populated liveColumns")
	}
	if _, ok := storeB.liveColumns["orders"]; !ok {
		t.Fatal("expected the preloaded live map to include the table storeA created")
	}

	// A schema with an extra column not present live must still fall
	// through to reconcileSchema (unchanged behaviour) and actually add
	// the column -- the preload fast path must never mask a real
	// migration need.
	widerSchema := &types.TableSchema{
		Name:      "orders",
		TableType: types.TableTypeLog,
		Columns: append(append([]types.Column{}, sch.Columns...),
			types.Column{Name: "new_field", Type: "string"}),
	}
	changed, err = storeB.CreateTableReport(ctx, widerSchema)
	if err != nil {
		t.Fatalf("storeB widen table: %v", err)
	}
	if !changed {
		t.Fatal("expected adding a genuinely new column to report changed=true")
	}

	// Verify with a third, fresh store that the column really landed on
	// the live table (not just in storeB's own cache) -- its own preload
	// must see new_field and take the fast path too.
	storeC := newStoreFromConnString(t, connStr)
	changed, err = storeC.CreateTableReport(ctx, widerSchema)
	if err != nil {
		t.Fatalf("storeC create table: %v", err)
	}
	if changed {
		t.Fatal("expected storeC's preload to already see new_field and report changed=false")
	}
}

// TestCreateTableReport_PreloadCoversViewTable exercises the same
// CreateTable path engine.go uses for view-result tables (Engine.setup's
// second loop, over ViewPoller.Setup's output) -- there is no separate
// store method for view tables, so the preload fast path applies to them
// automatically. This pins that down for a table shaped like a view result
// (no log_index column).
func TestCreateTableReport_PreloadCoversViewTable(t *testing.T) {
	connStr := newTestPostgresConnString(t)
	ctx := context.Background()

	storeA := newStoreFromConnString(t, connStr)
	viewSchema := &types.TableSchema{
		Name:      "get_active_deployment_view",
		TableType: types.TableTypeUnique,
		UniqueKey: "contract_address",
		Columns: []types.Column{
			{Name: "block_number", Type: "uint64"},
			{Name: "contract_address", Type: "string"},
			{Name: "active", Type: "bool"},
		},
	}
	if _, err := storeA.CreateTableReport(ctx, viewSchema); err != nil {
		t.Fatalf("storeA create view table: %v", err)
	}

	storeB := newStoreFromConnString(t, connStr)
	changed, err := storeB.CreateTableReport(ctx, viewSchema)
	if err != nil {
		t.Fatalf("storeB create view table: %v", err)
	}
	if changed {
		t.Fatal("expected the view table to resolve via the preload fast path too, changed=false")
	}
}
