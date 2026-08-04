package postgres

import (
	"context"
	"fmt"
	"strings"

	atlasschema "ariga.io/atlas/sql/schema"

	"github.com/b-j-roberts/ibis/internal/types"
)

const atlasInspectSchema = "public"

// hasAllColumns reports whether every column in incoming is already present
// in known, by name AND type. A cached schema that's missing a column
// incoming has (e.g. a later dynamic child's event has a field an earlier
// one didn't), or that has the same column name mapped to a different type
// (two child ABIs disagreeing on a shared field, e.g. u64 vs u256), means
// the live table needs reconciliation, not a skip -- a name-only match
// would let CreateTable's fast path apply the wrong cached type to the
// second child's inserts silently, instead of routing through
// reconcileSchema's incompatible-schema guard.
func hasAllColumns(known, incoming *types.TableSchema) bool {
	existing := make(map[string]types.Column, len(known.Columns))
	for _, col := range known.Columns {
		existing[col.Name] = col
	}
	for _, col := range incoming.Columns {
		match, ok := existing[col.Name]
		if !ok || match.Type != col.Type {
			return false
		}
	}
	return true
}

// reconcileSchema brings the live table for sch up to date and returns the
// schema to cache. Shared/factory tables are registered once per dynamic
// child mapping to the same table, and those children's ABIs can differ --
// this diffs the live table against the desired one via Atlas and only ever
// adds columns, rather than trusting whichever child's schema happened to
// create the table first.
func (s *PostgresStore) reconcileSchema(ctx context.Context, sch *types.TableSchema) (*types.TableSchema, error) {
	live, err := s.inspectLiveTable(ctx, sch.Name)
	if err != nil {
		return nil, fmt.Errorf("inspecting live table %s: %w", sch.Name, err)
	}

	if live == nil {
		ddl := s.generateCreateTableDDL(sch)
		if _, err := s.pool.Exec(ctx, ddl); err != nil && !isConcurrentCreateRace(err) {
			return nil, fmt.Errorf("creating table %s: %w", sch.Name, err)
		}

		// A concurrent CreateTable for this same (new) table may have won
		// the race with a different column set than ours -- CREATE TABLE IF
		// NOT EXISTS silently no-ops for the loser, so sch is not
		// necessarily what's now on disk. Re-inspect and fall through to the
		// same diff-and-reconcile path used for pre-existing tables instead
		// of trusting our own schema unconditionally.
		live, err = s.inspectLiveTable(ctx, sch.Name)
		if err != nil {
			return nil, fmt.Errorf("inspecting live table %s after create: %w", sch.Name, err)
		}
	}

	if err := s.ensureAggTable(ctx, sch); err != nil {
		return nil, err
	}

	desired := toAtlasTable(sch)
	changes, err := s.atlas.TableDiff(live, desired)
	if err != nil {
		return nil, fmt.Errorf("diffing schema for %s: %w", sch.Name, err)
	}

	var toAdd []*atlasschema.AddColumn
	for _, ch := range changes {
		if add, ok := ch.(*atlasschema.AddColumn); ok {
			toAdd = append(toAdd, add)
			continue
		}
		if strings.HasPrefix(fmt.Sprintf("%T", ch), "*schema.Drop") {
			// Columns/indexes present live but absent from this child's own
			// ABI-derived schema are expected under the factory model --
			// each dynamic child only describes its own event's fields.
			// Never drop anything automatically.
			continue
		}
		return nil, fmt.Errorf(
			"incompatible schema for shared table %s: %s -- refusing to auto-migrate a live table; add a manual migration instead",
			sch.Name, describeChange(ch),
		)
	}

	// Deliberately plain SQL rather than atlas's ApplyChanges: Atlas's
	// generated ADD COLUMN DDL has no IF NOT EXISTS guard, so it isn't safe
	// against a concurrent dynamic child reconciling the same table at the
	// same time. ALTER TABLE ... ADD COLUMN IF NOT EXISTS is idempotent,
	// matching MigrateTable's existing pattern below.
	for _, add := range toAdd {
		col, ok := findColumn(sch, add.C.Name)
		if !ok {
			// desired was built 1:1 from sch.Columns, so every AddColumn
			// Atlas reports must already be in sch.Columns -- this is a
			// should-never-happen internal inconsistency, not a routine
			// case to skip past silently.
			return nil, fmt.Errorf(
				"internal inconsistency reconciling %s: atlas wants to add column %q which is not present in the schema being registered",
				sch.Name, add.C.Name,
			)
		}
		query := fmt.Sprintf("ALTER TABLE %s ADD COLUMN IF NOT EXISTS %s %s",
			sch.Name, qid(col.Name), columnTypeToPostgres(col.Type))
		if _, err := s.pool.Exec(ctx, query); err != nil {
			return nil, fmt.Errorf("adding column %s to %s: %w", col.Name, sch.Name, err)
		}
	}

	return mergeSchema(live, sch), nil
}

// ensureAggTable creates the aggregation companion table for sch if it needs
// one. Both the new-table and existing-table reconciliation paths call this:
// a dynamic child can be the first to register an aggregation schema for a
// shared table another child already created.
func (s *PostgresStore) ensureAggTable(ctx context.Context, sch *types.TableSchema) error {
	if sch.TableType != types.TableTypeAggregation || len(sch.Aggregates) == 0 {
		return nil
	}
	aggDDL := s.generateAggTableDDL(sch)
	if _, err := s.pool.Exec(ctx, aggDDL); err != nil && !isConcurrentCreateRace(err) {
		return fmt.Errorf("creating agg table for %s: %w", sch.Name, err)
	}
	return nil
}

func findColumn(sch *types.TableSchema, name string) (types.Column, bool) {
	for _, col := range sch.Columns {
		if col.Name == name {
			return col, true
		}
	}
	return types.Column{}, false
}

func (s *PostgresStore) inspectLiveTable(ctx context.Context, name string) (*atlasschema.Table, error) {
	insp, err := s.atlas.InspectSchema(ctx, atlasInspectSchema, &atlasschema.InspectOptions{
		Tables: []string{name},
	})
	if err != nil {
		return nil, err
	}
	t, ok := insp.Table(name)
	if !ok {
		return nil, nil
	}
	return t, nil
}

func toAtlasTable(sch *types.TableSchema) *atlasschema.Table {
	t := atlasschema.NewTable(sch.Name)
	for _, col := range sch.Columns {
		t.AddColumns(toAtlasColumn(col))
	}
	return t
}

func toAtlasColumn(col types.Column) *atlasschema.Column {
	pgType := columnTypeToPostgres(col.Type)
	notNull := isNotNullColumn(col)
	switch col.Type {
	case "uint64", "int64":
		if notNull {
			return atlasschema.NewIntColumn(col.Name, pgType)
		}
		return atlasschema.NewNullIntColumn(col.Name, pgType)
	case "bool":
		if notNull {
			return atlasschema.NewBoolColumn(col.Name, pgType)
		}
		return atlasschema.NewNullBoolColumn(col.Name, pgType)
	case "[]byte":
		if notNull {
			return atlasschema.NewBinaryColumn(col.Name, pgType)
		}
		return atlasschema.NewNullBinaryColumn(col.Name, pgType)
	default:
		if notNull {
			return atlasschema.NewStringColumn(col.Name, pgType)
		}
		return atlasschema.NewNullStringColumn(col.Name, pgType)
	}
}

// isNotNullColumn mirrors generateCreateTableDDL's NOT NULL rule exactly --
// the diff is only meaningful if the desired-state model matches the DDL we
// actually emit.
func isNotNullColumn(col types.Column) bool {
	return !col.Nullable && (col.Name == "block_number" || col.Name == "log_index")
}

// describeChange renders a human-actionable description of an Atlas schema
// change for the refuse-to-migrate error path.
func describeChange(ch atlasschema.Change) string {
	switch c := ch.(type) {
	case *atlasschema.ModifyColumn:
		return fmt.Sprintf("column %q would change (%s)", c.To.Name, c.Change)
	case *atlasschema.AddIndex:
		return fmt.Sprintf("index %q would be added", c.I.Name)
	case *atlasschema.AddForeignKey:
		return fmt.Sprintf("foreign key %q would be added", c.F.Symbol)
	case *atlasschema.AddCheck:
		return "a check constraint would be added"
	case *atlasschema.RenameColumn:
		return fmt.Sprintf("column %q would be renamed to %q", c.From.Name, c.To.Name)
	default:
		return fmt.Sprintf("a %T change would be needed", ch)
	}
}

// mergeSchema combines the live table's columns with sch's own columns, so
// the cached schema reflects every column the table actually has -- not just
// the subset the registering child's own event happens to describe.
func mergeSchema(live *atlasschema.Table, sch *types.TableSchema) *types.TableSchema {
	merged := *sch

	seen := make(map[string]bool, len(sch.Columns))
	for _, col := range sch.Columns {
		seen[col.Name] = true
	}

	cols := make([]types.Column, len(sch.Columns))
	copy(cols, sch.Columns)
	for _, lc := range live.Columns {
		if seen[lc.Name] {
			continue
		}
		cols = append(cols, types.Column{
			Name:     lc.Name,
			Type:     rawPostgresTypeToColumnType(lc.Type.Raw),
			Nullable: lc.Type.Null,
		})
	}

	merged.Columns = cols
	return &merged
}

func rawPostgresTypeToColumnType(raw string) string {
	switch strings.ToLower(raw) {
	case "bigint", "int8":
		return "int64"
	case "boolean", "bool":
		return "bool"
	case "bytea":
		return "[]byte"
	default:
		return "string"
	}
}
