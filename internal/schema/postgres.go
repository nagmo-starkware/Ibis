package schema

import (
	"fmt"
	"strings"

	"github.com/b-j-roberts/ibis/internal/types"
)

// qid quotes a PostgreSQL identifier to safely handle reserved words (e.g. "from", "to").
func qid(name string) string {
	return `"` + name + `"`
}

// columnTypeToPostgres maps Go column type strings to PostgreSQL column types.
func columnTypeToPostgres(colType string) string {
	switch colType {
	case "uint64":
		return "BIGINT"
	case "int64":
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

// GenerateCreateTableSQL generates a PostgreSQL CREATE TABLE statement from a TableSchema.
// Includes appropriate indices for the table type.
func GenerateCreateTableSQL(schema *types.TableSchema) string {
	var b strings.Builder

	b.WriteString(fmt.Sprintf("CREATE TABLE IF NOT EXISTS %s (\n", schema.Name))

	// Columns.
	for i, col := range schema.Columns {
		pgType := columnTypeToPostgres(col.Type)
		nullable := ""
		if !col.Nullable && (col.Name == "block_number" || col.Name == "log_index") {
			nullable = " NOT NULL"
		}
		b.WriteString(fmt.Sprintf("    \"%s\" %s%s", col.Name, pgType, nullable))
		if i < len(schema.Columns)-1 {
			b.WriteString(",")
		}
		b.WriteString("\n")
	}

	b.WriteString(");\n")

	// Standard index on block_number for all tables.
	b.WriteString(fmt.Sprintf("\nCREATE INDEX IF NOT EXISTS idx_%s_block ON %s (%s);\n",
		schema.Name, schema.Name, qid("block_number")))

	// Composite index for event ordering.
	b.WriteString(fmt.Sprintf("CREATE INDEX IF NOT EXISTS idx_%s_block_log ON %s (%s, %s);\n",
		schema.Name, schema.Name, qid("block_number"), qid("log_index")))

	// Unique index for unique tables.
	if schema.TableType == types.TableTypeUnique && schema.UniqueKey != "" {
		if schema.SharedTable {
			// Composite unique constraint: (contract_address, unique_key).
			b.WriteString(fmt.Sprintf("CREATE UNIQUE INDEX IF NOT EXISTS idx_%s_unique_%s ON %s (%s, %s);\n",
				schema.Name, schema.UniqueKey, schema.Name, qid("contract_address"), qid(schema.UniqueKey)))
		} else {
			b.WriteString(fmt.Sprintf("CREATE UNIQUE INDEX IF NOT EXISTS idx_%s_unique_%s ON %s (%s);\n",
				schema.Name, schema.UniqueKey, schema.Name, qid(schema.UniqueKey)))
		}
	}

	// Index on contract_address for efficient per-child filtering in shared tables.
	if schema.SharedTable {
		b.WriteString(fmt.Sprintf("CREATE INDEX IF NOT EXISTS idx_%s_contract ON %s (%s);\n",
			schema.Name, schema.Name, qid("contract_address")))
	}

	// Status index for filtering by confirmation status.
	b.WriteString(fmt.Sprintf("CREATE INDEX IF NOT EXISTS idx_%s_status ON %s (%s);\n",
		schema.Name, schema.Name, qid("status")))

	return b.String()
}

// GenerateAggregationTableSQL generates the companion aggregation tracking table
// for aggregation-type schemas.
func GenerateAggregationTableSQL(schema *types.TableSchema) string {
	if schema.TableType != types.TableTypeAggregation || len(schema.Aggregates) == 0 {
		return ""
	}

	aggTableName := schema.Name + "_agg"
	var b strings.Builder

	b.WriteString(fmt.Sprintf("CREATE TABLE IF NOT EXISTS %s (\n", aggTableName))
	b.WriteString("    id SERIAL PRIMARY KEY,\n")

	for i, agg := range schema.Aggregates {
		pgType := "DOUBLE PRECISION"
		if agg.Operation == "count" {
			pgType = "BIGINT"
		}
		b.WriteString(fmt.Sprintf("    %s %s DEFAULT 0", qid(agg.Column), pgType))
		if i < len(schema.Aggregates)-1 {
			b.WriteString(",")
		}
		b.WriteString("\n")
	}

	b.WriteString(");\n")

	return b.String()
}
