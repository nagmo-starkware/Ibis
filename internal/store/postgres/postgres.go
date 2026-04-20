package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/b-j-roberts/ibis/internal/config"
	"github.com/b-j-roberts/ibis/internal/store"
	"github.com/b-j-roberts/ibis/internal/types"
)

// PostgresStore implements store.Store using PostgreSQL via pgx/v5.
type PostgresStore struct {
	pool    *pgxpool.Pool
	schemas map[string]types.TableSchema
}

// New creates a new PostgresStore from the given config.
func New(ctx context.Context, cfg config.PostgresConfig) (*PostgresStore, error) {
	connStr := fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=disable",
		cfg.User, cfg.Password, cfg.Host, cfg.Port, cfg.Name)

	pool, err := pgxpool.New(ctx, connStr)
	if err != nil {
		return nil, fmt.Errorf("connecting to postgres: %w", err)
	}

	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("pinging postgres: %w", err)
	}

	s := &PostgresStore{
		pool:    pool,
		schemas: make(map[string]types.TableSchema),
	}

	if err := s.ensureMetaTable(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("creating meta table: %w", err)
	}

	return s, nil
}

// NewFromPool creates a PostgresStore from an existing connection pool (for testing).
func NewFromPool(ctx context.Context, pool *pgxpool.Pool) (*PostgresStore, error) {
	s := &PostgresStore{
		pool:    pool,
		schemas: make(map[string]types.TableSchema),
	}

	if err := s.ensureMetaTable(ctx); err != nil {
		return nil, fmt.Errorf("creating meta table: %w", err)
	}

	return s, nil
}

func (s *PostgresStore) ensureMetaTable(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS _ibis_meta (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL
		)
	`)
	if err != nil {
		return err
	}

	_, err = s.pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS _ibis_cursors (
			contract_name TEXT PRIMARY KEY,
			last_block BIGINT NOT NULL DEFAULT 0,
			updated_at TIMESTAMP NOT NULL DEFAULT NOW()
		)
	`)
	if err != nil {
		return err
	}

	_, err = s.pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS _ibis_dynamic_contracts (
			name TEXT PRIMARY KEY,
			config_json JSONB NOT NULL,
			created_at TIMESTAMP NOT NULL DEFAULT NOW(),
			updated_at TIMESTAMP NOT NULL DEFAULT NOW()
		)
	`)
	return err
}

// ---- Store interface ----

func (s *PostgresStore) ApplyOperations(ctx context.Context, ops []store.Operation) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback after commit is a no-op

	// Collect aggregation deltas per table.
	aggDeltas := make(map[string][]aggDelta)

	for _, op := range ops {
		if err := s.applyOp(ctx, tx, op, aggDeltas); err != nil {
			return fmt.Errorf("applying %s on %s: %w", op.Type, op.Table, err)
		}
	}

	// Apply aggregation deltas.
	for table, deltas := range aggDeltas {
		if err := s.applyAggDeltas(ctx, tx, table, deltas); err != nil {
			return fmt.Errorf("applying aggregation for %s: %w", table, err)
		}
	}

	return tx.Commit(ctx)
}

type aggDelta struct {
	specs    []types.AggregateSpec
	data     map[string]any
	subtract bool
}

func (s *PostgresStore) applyOp(ctx context.Context, tx pgx.Tx, op store.Operation, aggDeltas map[string][]aggDelta) error {
	sch, hasSchema := s.schemas[op.Table]

	switch op.Type {
	case store.OpInsert:
		if err := s.insertRow(ctx, tx, op); err != nil {
			return err
		}
		// For unique tables, the insert already handles upsert via ON CONFLICT.
		if hasSchema && sch.TableType == types.TableTypeAggregation {
			aggDeltas[op.Table] = append(aggDeltas[op.Table], aggDelta{
				specs: sch.Aggregates, data: op.Data, subtract: false,
			})
		}

	case store.OpUpdate:
		if err := s.updateRow(ctx, tx, op); err != nil {
			return err
		}

	case store.OpDelete:
		if err := s.deleteRow(ctx, tx, op); err != nil {
			return err
		}
		if hasSchema && sch.TableType == types.TableTypeAggregation {
			aggDeltas[op.Table] = append(aggDeltas[op.Table], aggDelta{
				specs: sch.Aggregates, data: op.Data, subtract: true,
			})
		}
	}

	return nil
}

func (s *PostgresStore) insertRow(ctx context.Context, tx pgx.Tx, op store.Operation) error {
	if len(op.Data) == 0 {
		return nil
	}

	sch, hasSchema := s.schemas[op.Table]

	// Build column list from schema if available, otherwise from data keys.
	var cols []string
	var vals []any
	var placeholders []string

	if hasSchema {
		for _, col := range sch.Columns {
			v, ok := op.Data[col.Name]
			if !ok {
				continue
			}
			cols = append(cols, qid(col.Name))
			vals = append(vals, convertValue(v, col.Type))
			placeholders = append(placeholders, fmt.Sprintf("$%d", len(vals)))
		}
	} else {
		for k, v := range op.Data {
			cols = append(cols, qid(k))
			vals = append(vals, v)
			placeholders = append(placeholders, fmt.Sprintf("$%d", len(vals)))
		}
	}

	if len(cols) == 0 {
		return nil
	}

	query := fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)",
		op.Table, strings.Join(cols, ", "), strings.Join(placeholders, ", "))

	// For unique tables, use ON CONFLICT to upsert.
	if hasSchema && sch.TableType == types.TableTypeUnique && sch.UniqueKey != "" {
		// Shared tables use composite unique key: (contract_address, unique_key).
		conflictCols := qid(sch.UniqueKey)
		if sch.SharedTable {
			conflictCols = qid("contract_address") + ", " + qid(sch.UniqueKey)
		}

		setClauses := make([]string, 0, len(cols))
		for _, col := range cols {
			if col == qid(sch.UniqueKey) {
				continue
			}
			if sch.SharedTable && col == qid("contract_address") {
				continue
			}
			setClauses = append(setClauses, fmt.Sprintf("%s = EXCLUDED.%s", col, col))
		}
		if len(setClauses) > 0 {
			query += fmt.Sprintf(" ON CONFLICT (%s) DO UPDATE SET %s",
				conflictCols, strings.Join(setClauses, ", "))
		}
	}

	_, err := tx.Exec(ctx, query, vals...)
	return err
}

func (s *PostgresStore) updateRow(ctx context.Context, tx pgx.Tx, op store.Operation) error {
	if len(op.Data) == 0 {
		return nil
	}

	sch, hasSchema := s.schemas[op.Table]

	var setClauses []string
	var vals []any
	idx := 1

	if hasSchema {
		for _, col := range sch.Columns {
			v, ok := op.Data[col.Name]
			if !ok {
				continue
			}
			// Don't update the key columns used in WHERE.
			if col.Name == "block_number" || col.Name == "log_index" {
				continue
			}
			setClauses = append(setClauses, fmt.Sprintf("%s = $%d", qid(col.Name), idx))
			vals = append(vals, convertValue(v, col.Type))
			idx++
		}
	} else {
		for k, v := range op.Data {
			if k == "block_number" || k == "log_index" {
				continue
			}
			setClauses = append(setClauses, fmt.Sprintf("%s = $%d", qid(k), idx))
			vals = append(vals, v)
			idx++
		}
	}

	if len(setClauses) == 0 {
		return nil
	}

	vals = append(vals, op.BlockNumber, op.LogIndex)
	query := fmt.Sprintf("UPDATE %s SET %s WHERE %s = $%d AND %s = $%d",
		op.Table, strings.Join(setClauses, ", "), qid("block_number"), idx, qid("log_index"), idx+1)

	_, err := tx.Exec(ctx, query, vals...)
	return err
}

func (s *PostgresStore) deleteRow(ctx context.Context, tx pgx.Tx, op store.Operation) error {
	query := fmt.Sprintf("DELETE FROM %s WHERE %s = $1 AND %s = $2", op.Table, qid("block_number"), qid("log_index"))
	_, err := tx.Exec(ctx, query, op.BlockNumber, op.LogIndex)
	return err
}

func (s *PostgresStore) applyAggDeltas(ctx context.Context, tx pgx.Tx, table string, deltas []aggDelta) error {
	aggTable := table + "_agg"

	// Read current values. The agg table has a single row with id=1.
	var exists bool
	err := tx.QueryRow(ctx, fmt.Sprintf("SELECT EXISTS(SELECT 1 FROM %s WHERE id = 1)", aggTable)).Scan(&exists)
	if err != nil {
		return fmt.Errorf("checking agg row: %w", err)
	}

	if !exists {
		// Insert initial row.
		_, err := tx.Exec(ctx, fmt.Sprintf("INSERT INTO %s (id) VALUES (1)", aggTable))
		if err != nil {
			return fmt.Errorf("inserting agg row: %w", err)
		}
	}

	// Build SET clauses for all deltas.
	// Collect the column updates: for sum/count, use SQL arithmetic.
	colUpdates := make(map[string]float64)
	for _, d := range deltas {
		for _, spec := range d.specs {
			switch spec.Operation {
			case "sum":
				val := toFloat64(d.data[spec.Field])
				if d.subtract {
					colUpdates[spec.Column] -= val
				} else {
					colUpdates[spec.Column] += val
				}
			case "count":
				if d.subtract {
					colUpdates[spec.Column]--
				} else {
					colUpdates[spec.Column]++
				}
			case "avg":
				// For avg, track sum and count in separate columns.
				sumCol := spec.Column + "__sum"
				cntCol := spec.Column + "__count"
				val := toFloat64(d.data[spec.Field])
				if d.subtract {
					colUpdates[sumCol] -= val
					colUpdates[cntCol]--
				} else {
					colUpdates[sumCol] += val
					colUpdates[cntCol]++
				}
			}
		}
	}

	if len(colUpdates) == 0 {
		return nil
	}

	var setClauses []string
	var vals []any
	idx := 1
	for col, delta := range colUpdates {
		setClauses = append(setClauses, fmt.Sprintf("%s = %s + $%d", qid(col), qid(col), idx))
		vals = append(vals, delta)
		idx++
	}

	query := fmt.Sprintf("UPDATE %s SET %s WHERE id = 1", aggTable, strings.Join(setClauses, ", "))
	_, err = tx.Exec(ctx, query, vals...)
	if err != nil {
		return fmt.Errorf("updating agg: %w", err)
	}

	// For avg columns, compute the actual average.
	// Find any avg specs in the deltas.
	for _, d := range deltas {
		for _, spec := range d.specs {
			if spec.Operation == "avg" {
				sumCol := spec.Column + "__sum"
				cntCol := spec.Column + "__count"
				avgQuery := fmt.Sprintf(
					"UPDATE %s SET %s = CASE WHEN %s > 0 THEN %s / %s ELSE 0 END WHERE id = 1",
					aggTable, qid(spec.Column), qid(cntCol), qid(sumCol), qid(cntCol),
				)
				if _, err := tx.Exec(ctx, avgQuery); err != nil {
					return fmt.Errorf("computing avg for %s: %w", spec.Column, err)
				}
			}
		}
	}

	return nil
}

func (s *PostgresStore) RevertOperations(ctx context.Context, ops []store.Operation) error {
	reversed := make([]store.Operation, len(ops))
	for i, op := range ops {
		reversed[len(ops)-1-i] = op.InverseOp()
	}
	return s.ApplyOperations(ctx, reversed)
}

func (s *PostgresStore) GetEvents(ctx context.Context, table string, q store.Query) ([]types.IndexedEvent, error) {
	query, args := s.buildSelectQuery(table, q)

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("querying events: %w", err)
	}
	defer rows.Close()

	return s.scanEvents(rows, table)
}

func (s *PostgresStore) GetUniqueEvents(ctx context.Context, table string, q store.Query) ([]types.IndexedEvent, error) {
	sch, ok := s.schemas[table]
	if !ok || sch.UniqueKey == "" {
		return s.GetEvents(ctx, table, q)
	}

	// Use DISTINCT ON to get latest entry per unique key.
	query, args := s.buildUniqueSelectQuery(table, sch.UniqueKey, q)

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("querying unique events: %w", err)
	}
	defer rows.Close()

	return s.scanEvents(rows, table)
}

func (s *PostgresStore) GetAggregation(ctx context.Context, table string, _ store.Query) (store.AggResult, error) {
	result := store.AggResult{Values: make(map[string]any)}
	aggTable := table + "_agg"

	sch, ok := s.schemas[table]
	if !ok {
		return result, nil
	}

	// Build column list from aggregate specs, excluding internal __sum/__count.
	var cols []string
	var rawCols []string
	for _, agg := range sch.Aggregates {
		cols = append(cols, qid(agg.Column))
		rawCols = append(rawCols, agg.Column)
	}

	if len(cols) == 0 {
		return result, nil
	}

	query := fmt.Sprintf("SELECT %s FROM %s WHERE id = 1", strings.Join(cols, ", "), aggTable)

	row := s.pool.QueryRow(ctx, query)
	scanDest := make([]any, len(cols))
	scanPtrs := make([]any, len(cols))
	for i := range scanDest {
		scanPtrs[i] = &scanDest[i]
	}

	if err := row.Scan(scanPtrs...); err != nil {
		if err == pgx.ErrNoRows {
			return result, nil
		}
		return result, fmt.Errorf("scanning aggregation: %w", err)
	}

	for i, col := range rawCols {
		result.Values[col] = toFloat64(scanDest[i])
	}

	return result, nil
}

func (s *PostgresStore) GetCursor(ctx context.Context, contract string) (uint64, error) {
	var lastBlock int64
	err := s.pool.QueryRow(ctx,
		"SELECT last_block FROM _ibis_cursors WHERE contract_name = $1", contract).Scan(&lastBlock)
	if err != nil {
		if err == pgx.ErrNoRows {
			return 0, nil
		}
		return 0, fmt.Errorf("getting cursor for %s: %w", contract, err)
	}
	return uint64(lastBlock), nil
}

func (s *PostgresStore) SetCursor(ctx context.Context, contract string, blockNumber uint64) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO _ibis_cursors (contract_name, last_block, updated_at)
		 VALUES ($1, $2, NOW())
		 ON CONFLICT (contract_name) DO UPDATE SET last_block = $2, updated_at = NOW()`,
		contract, int64(blockNumber))
	return err
}

func (s *PostgresStore) GetAllCursors(ctx context.Context) (map[string]uint64, error) {
	rows, err := s.pool.Query(ctx, "SELECT contract_name, last_block FROM _ibis_cursors")
	if err != nil {
		return nil, fmt.Errorf("getting all cursors: %w", err)
	}
	defer rows.Close()

	result := make(map[string]uint64)
	for rows.Next() {
		var name string
		var lastBlock int64
		if err := rows.Scan(&name, &lastBlock); err != nil {
			return nil, fmt.Errorf("scanning cursor row: %w", err)
		}
		result[name] = uint64(lastBlock)
	}
	return result, rows.Err()
}

func (s *PostgresStore) CreateTable(ctx context.Context, sch *types.TableSchema) error {
	s.schemas[sch.Name] = *sch

	// Generate CREATE TABLE directly from schema columns.
	ddl := s.generateCreateTableDDL(sch)
	if _, err := s.pool.Exec(ctx, ddl); err != nil {
		return fmt.Errorf("creating table %s: %w", sch.Name, err)
	}

	// Create aggregation companion table if needed.
	if sch.TableType == types.TableTypeAggregation && len(sch.Aggregates) > 0 {
		aggDDL := s.generateAggTableDDL(sch)
		if _, err := s.pool.Exec(ctx, aggDDL); err != nil {
			return fmt.Errorf("creating agg table for %s: %w", sch.Name, err)
		}
	}

	return nil
}

// generateCreateTableDDL builds CREATE TABLE SQL from the schema's actual columns.
func (s *PostgresStore) generateCreateTableDDL(sch *types.TableSchema) string {
	var b strings.Builder

	b.WriteString(fmt.Sprintf("CREATE TABLE IF NOT EXISTS %s (\n", sch.Name))

	for i, col := range sch.Columns {
		pgType := columnTypeToPostgres(col.Type)
		nullable := ""
		if !col.Nullable && (col.Name == "block_number" || col.Name == "log_index") {
			nullable = " NOT NULL"
		}
		b.WriteString(fmt.Sprintf("    %s %s%s", qid(col.Name), pgType, nullable))
		if i < len(sch.Columns)-1 {
			b.WriteString(",")
		}
		b.WriteString("\n")
	}

	b.WriteString(");\n")

	// Only create indices for columns that exist.
	colSet := make(map[string]bool)
	for _, col := range sch.Columns {
		colSet[col.Name] = true
	}

	if colSet["block_number"] {
		b.WriteString(fmt.Sprintf("\nCREATE INDEX IF NOT EXISTS idx_%s_block ON %s (%s);\n",
			sch.Name, sch.Name, qid("block_number")))
	}
	if colSet["block_number"] && colSet["log_index"] {
		b.WriteString(fmt.Sprintf("CREATE INDEX IF NOT EXISTS idx_%s_block_log ON %s (%s, %s);\n",
			sch.Name, sch.Name, qid("block_number"), qid("log_index")))
	}
	if sch.TableType == types.TableTypeUnique && sch.UniqueKey != "" && colSet[sch.UniqueKey] {
		if sch.SharedTable {
			b.WriteString(fmt.Sprintf("CREATE UNIQUE INDEX IF NOT EXISTS idx_%s_unique_%s ON %s (%s, %s);\n",
				sch.Name, sch.UniqueKey, sch.Name, qid("contract_address"), qid(sch.UniqueKey)))
		} else {
			b.WriteString(fmt.Sprintf("CREATE UNIQUE INDEX IF NOT EXISTS idx_%s_unique_%s ON %s (%s);\n",
				sch.Name, sch.UniqueKey, sch.Name, qid(sch.UniqueKey)))
		}
	}
	if sch.SharedTable && colSet["contract_address"] {
		b.WriteString(fmt.Sprintf("CREATE INDEX IF NOT EXISTS idx_%s_contract ON %s (%s);\n",
			sch.Name, sch.Name, qid("contract_address")))
	}
	if colSet["status"] {
		b.WriteString(fmt.Sprintf("CREATE INDEX IF NOT EXISTS idx_%s_status ON %s (%s);\n",
			sch.Name, sch.Name, qid("status")))
	}

	return b.String()
}

// generateAggTableDDL builds the companion aggregation table DDL.
func (s *PostgresStore) generateAggTableDDL(sch *types.TableSchema) string {
	aggTable := sch.Name + "_agg"
	var b strings.Builder

	b.WriteString(fmt.Sprintf("CREATE TABLE IF NOT EXISTS %s (\n", aggTable))
	b.WriteString("    id SERIAL PRIMARY KEY")

	for _, agg := range sch.Aggregates {
		pgType := "DOUBLE PRECISION"
		if agg.Operation == "count" {
			pgType = "BIGINT"
		}
		b.WriteString(fmt.Sprintf(",\n    %s %s DEFAULT 0", qid(agg.Column), pgType))

		// For avg, add internal tracking columns.
		if agg.Operation == "avg" {
			b.WriteString(fmt.Sprintf(",\n    %s DOUBLE PRECISION DEFAULT 0", qid(agg.Column+"__sum")))
			b.WriteString(fmt.Sprintf(",\n    %s DOUBLE PRECISION DEFAULT 0", qid(agg.Column+"__count")))
		}
	}

	b.WriteString("\n);\n")
	return b.String()
}

func (s *PostgresStore) MigrateTable(ctx context.Context, sch *types.TableSchema) error {
	oldSchema, exists := s.schemas[sch.Name]
	if !exists {
		return s.CreateTable(ctx, sch)
	}

	// Find new columns to add (never drop).
	existingCols := make(map[string]bool)
	for _, col := range oldSchema.Columns {
		existingCols[col.Name] = true
	}

	for _, col := range sch.Columns {
		if !existingCols[col.Name] {
			pgType := columnTypeToPostgres(col.Type)
			query := fmt.Sprintf("ALTER TABLE %s ADD COLUMN IF NOT EXISTS %s %s",
				sch.Name, qid(col.Name), pgType)
			if _, err := s.pool.Exec(ctx, query); err != nil {
				return fmt.Errorf("adding column %s to %s: %w", col.Name, sch.Name, err)
			}
		}
	}

	s.schemas[sch.Name] = *sch
	return nil
}

func (s *PostgresStore) CountEvents(ctx context.Context, table string, filters []store.Filter) (int64, error) {
	var args []any
	argIdx := 1
	where := s.buildWhereClause(filters, &args, &argIdx)

	query := fmt.Sprintf("SELECT COUNT(*) FROM %s%s", table, where)

	var count int64
	err := s.pool.QueryRow(ctx, query, args...).Scan(&count)
	return count, err
}

func (s *PostgresStore) DropTable(ctx context.Context, tableName string) error {
	delete(s.schemas, tableName)

	// Drop the main table and aggregation companion.
	_, err := s.pool.Exec(ctx, fmt.Sprintf("DROP TABLE IF EXISTS %s CASCADE", tableName))
	if err != nil {
		return fmt.Errorf("dropping table %s: %w", tableName, err)
	}
	_, err = s.pool.Exec(ctx, fmt.Sprintf("DROP TABLE IF EXISTS %s_agg CASCADE", tableName))
	if err != nil {
		return fmt.Errorf("dropping agg table for %s: %w", tableName, err)
	}
	return nil
}

func (s *PostgresStore) SaveDynamicContract(ctx context.Context, cc *config.ContractConfig) error {
	data, err := json.Marshal(cc)
	if err != nil {
		return fmt.Errorf("marshaling contract config: %w", err)
	}
	_, err = s.pool.Exec(ctx,
		`INSERT INTO _ibis_dynamic_contracts (name, config_json, updated_at)
		 VALUES ($1, $2, NOW())
		 ON CONFLICT (name) DO UPDATE SET config_json = $2, updated_at = NOW()`,
		cc.Name, data)
	return err
}

func (s *PostgresStore) GetDynamicContracts(ctx context.Context) ([]config.ContractConfig, error) {
	rows, err := s.pool.Query(ctx, "SELECT config_json FROM _ibis_dynamic_contracts")
	if err != nil {
		return nil, fmt.Errorf("querying dynamic contracts: %w", err)
	}
	defer rows.Close()

	var contracts []config.ContractConfig
	for rows.Next() {
		var data []byte
		if err := rows.Scan(&data); err != nil {
			return nil, fmt.Errorf("scanning dynamic contract: %w", err)
		}
		var cc config.ContractConfig
		if err := json.Unmarshal(data, &cc); err != nil {
			return nil, fmt.Errorf("unmarshaling contract config: %w", err)
		}
		cc.Dynamic = true
		contracts = append(contracts, cc)
	}
	return contracts, rows.Err()
}

func (s *PostgresStore) DeleteDynamicContract(ctx context.Context, name string) error {
	_, err := s.pool.Exec(ctx, "DELETE FROM _ibis_dynamic_contracts WHERE name = $1", name)
	return err
}

func (s *PostgresStore) DeleteCursor(ctx context.Context, contract string) error {
	_, err := s.pool.Exec(ctx, "DELETE FROM _ibis_cursors WHERE contract_name = $1", contract)
	return err
}

func (s *PostgresStore) Close() error {
	s.pool.Close()
	return nil
}

// ---- Query builders ----

func (s *PostgresStore) buildSelectQuery(table string, q store.Query) (query string, args []any) {
	sch, hasSchema := s.schemas[table]

	var cols string
	if hasSchema {
		colNames := make([]string, len(sch.Columns))
		for i, col := range sch.Columns {
			colNames[i] = qid(col.Name)
		}
		cols = strings.Join(colNames, ", ")
	} else {
		cols = "*"
	}

	argIdx := 1

	where := s.buildWhereClause(q.Filters, &args, &argIdx)

	orderBy := qid("block_number")
	if q.OrderBy != "" {
		orderBy = qid(q.OrderBy)
	}
	dir := "ASC"
	if q.OrderDir == store.OrderDesc {
		dir = "DESC"
	}
	// Secondary sort by log_index for stable ordering.
	orderClause := fmt.Sprintf("%s %s, %s %s", orderBy, dir, qid("log_index"), dir)

	limit := q.Limit
	if limit <= 0 {
		limit = 50
	}

	query = fmt.Sprintf("SELECT %s FROM %s%s ORDER BY %s LIMIT $%d OFFSET $%d",
		cols, table, where, orderClause, argIdx, argIdx+1)
	args = append(args, limit, q.Offset)

	return query, args
}

func (s *PostgresStore) buildUniqueSelectQuery(table, uniqueKey string, q store.Query) (query string, args []any) {
	sch, hasSchema := s.schemas[table]

	var cols string
	if hasSchema {
		colNames := make([]string, len(sch.Columns))
		for i, col := range sch.Columns {
			colNames[i] = qid(col.Name)
		}
		cols = strings.Join(colNames, ", ")
	} else {
		cols = "*"
	}

	argIdx := 1

	where := s.buildWhereClause(q.Filters, &args, &argIdx)

	orderBy := qid("block_number")
	if q.OrderBy != "" {
		orderBy = qid(q.OrderBy)
	}
	dir := "ASC"
	if q.OrderDir == store.OrderDesc {
		dir = "DESC"
	}

	limit := q.Limit
	if limit <= 0 {
		limit = 50
	}

	// For shared tables, use composite DISTINCT ON so each contract keeps its own row.
	distinctCols := qid(uniqueKey)
	if hasSchema && sch.SharedTable {
		distinctCols = qid("contract_address") + ", " + qid(uniqueKey)
	}

	// Use DISTINCT ON to get latest per unique key, then wrap for ordering/pagination.
	query = fmt.Sprintf(
		`SELECT %s FROM (
			SELECT DISTINCT ON (%s) %s
			FROM %s%s
			ORDER BY %s, %s DESC, %s DESC
		) sub ORDER BY %s %s, %s %s LIMIT $%d OFFSET $%d`,
		cols, distinctCols, cols, table, where,
		distinctCols, qid("block_number"), qid("log_index"),
		orderBy, dir, qid("log_index"), dir, argIdx, argIdx+1)
	args = append(args, limit, q.Offset)

	return query, args
}

func (s *PostgresStore) buildWhereClause(filters []store.Filter, args *[]any, argIdx *int) string {
	if len(filters) == 0 {
		return ""
	}

	var conditions []string
	for _, f := range filters {
		op := filterOpToSQL(f.Operator)
		conditions = append(conditions, fmt.Sprintf("%s %s $%d", qid(f.Field), op, *argIdx))
		*args = append(*args, fmt.Sprint(f.Value))
		(*argIdx)++
	}

	return " WHERE " + strings.Join(conditions, " AND ")
}

func filterOpToSQL(op string) string {
	switch op {
	case "eq":
		return "="
	case "neq":
		return "!="
	case "gt":
		return ">"
	case "gte":
		return ">="
	case "lt":
		return "<"
	case "lte":
		return "<="
	default:
		return "="
	}
}

func (s *PostgresStore) scanEvents(rows pgx.Rows, table string) ([]types.IndexedEvent, error) {
	sch, hasSchema := s.schemas[table]
	descs := rows.FieldDescriptions()

	var events []types.IndexedEvent
	for rows.Next() {
		vals, err := rows.Values()
		if err != nil {
			return nil, fmt.Errorf("scanning row: %w", err)
		}

		evt := types.IndexedEvent{
			Data: make(map[string]any, len(vals)),
		}

		for i, val := range vals {
			colName := descs[i].Name
			if hasSchema {
				val = convertScannedValue(val, columnType(&sch, colName))
			}
			evt.Data[colName] = val
		}

		populateFromData(&evt)
		events = append(events, evt)
	}

	return events, rows.Err()
}

// ---- Helpers ----

// qid quotes a PostgreSQL identifier to safely handle reserved words (e.g. "from", "to").
func qid(name string) string {
	return `"` + name + `"`
}

func columnType(sch *types.TableSchema, colName string) string {
	for _, col := range sch.Columns {
		if col.Name == colName {
			return col.Type
		}
	}
	return ""
}

func populateFromData(evt *types.IndexedEvent) {
	if evt.Data == nil {
		return
	}
	if v, ok := evt.Data["block_number"]; ok {
		evt.BlockNumber = toUint64(v)
	}
	if v, ok := evt.Data["log_index"]; ok {
		evt.LogIndex = toUint64(v)
	}
	if v, ok := evt.Data["transaction_hash"]; ok {
		evt.TransactionHash = fmt.Sprint(v)
	}
	if v, ok := evt.Data["contract_address"]; ok {
		evt.ContractAddress = fmt.Sprint(v)
	}
	if v, ok := evt.Data["event_name"]; ok {
		evt.EventName = fmt.Sprint(v)
	}
	if v, ok := evt.Data["timestamp"]; ok {
		evt.Timestamp = toUint64(v)
	}
}

func convertValue(v any, colType string) any {
	switch colType {
	case "uint64", "int64":
		return toInt64(v)
	case "bool":
		switch b := v.(type) {
		case bool:
			return b
		default:
			return fmt.Sprint(v) == "true"
		}
	case "string":
		return fmt.Sprint(v)
	case "[]byte":
		switch b := v.(type) {
		case []byte:
			return b
		case string:
			return []byte(b)
		default:
			return []byte(fmt.Sprint(v))
		}
	default:
		return v
	}
}

func convertScannedValue(v any, colType string) any {
	if v == nil {
		return nil
	}
	switch colType {
	case "uint64":
		return toUint64(v)
	case "int64":
		return toInt64(v)
	default:
		return v
	}
}

func columnTypeToPostgres(colType string) string {
	switch colType {
	case "uint64", "int64":
		return "BIGINT"
	case "string":
		return "TEXT"
	case "bool":
		return "BOOLEAN"
	case "[]byte":
		return "BYTEA"
	default:
		return "TEXT"
	}
}

func toFloat64(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case float32:
		return float64(n)
	case int:
		return float64(n)
	case int32:
		return float64(n)
	case int64:
		return float64(n)
	case uint64:
		return float64(n)
	case json.Number:
		f, _ := n.Float64()
		return f
	default:
		return 0
	}
}

func toUint64(v any) uint64 {
	switch n := v.(type) {
	case float64:
		return uint64(n)
	case float32:
		return uint64(n)
	case int:
		return uint64(n)
	case int32:
		return uint64(n)
	case int64:
		return uint64(n)
	case uint64:
		return n
	case json.Number:
		i, _ := n.Int64()
		return uint64(i)
	default:
		return 0
	}
}

func toInt64(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case float32:
		return int64(n)
	case int:
		return int64(n)
	case int32:
		return int64(n)
	case int64:
		return n
	case uint64:
		return int64(n)
	case json.Number:
		i, _ := n.Int64()
		return i
	default:
		return 0
	}
}
