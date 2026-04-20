package schema

import (
	"fmt"
	"strings"

	"github.com/b-j-roberts/ibis/internal/abi"
	"github.com/b-j-roberts/ibis/internal/config"
	"github.com/b-j-roberts/ibis/internal/types"
)

// MetadataColumns returns the standard metadata columns added to every table.
func MetadataColumns() []types.Column {
	return []types.Column{
		{Name: "block_number", Type: "uint64"},
		{Name: "transaction_hash", Type: "string"},
		{Name: "log_index", Type: "uint64"},
		{Name: "timestamp", Type: "uint64"},
		{Name: "contract_address", Type: "string"},
		{Name: "event_name", Type: "string"},
		{Name: "status", Type: "string"},
	}
}

// BuildOptions controls shared table behavior for factory children.
type BuildOptions struct {
	SharedTable bool   // Build schemas as shared tables
	FactoryName string // Factory name used for table naming
}

// BuildSchemas creates TableSchema definitions for a contract's configured events.
// Handles wildcard ("*") expansion: all ABI events get the wildcard's table type,
// with specific event entries overriding the default.
// Pass opts for factory children with shared tables; nil for normal contracts.
func BuildSchemas(cc *config.ContractConfig, contractABI *abi.ABI, registry *abi.EventRegistry, opts *BuildOptions) map[string]*types.TableSchema {
	schemas := make(map[string]*types.TableSchema)

	// Build a lookup of explicitly configured events.
	explicit := make(map[string]config.EventConfig)
	var wildcard *config.EventConfig
	for _, ec := range cc.Events {
		if ec.Name == "*" {
			ecCopy := ec
			wildcard = &ecCopy
		} else {
			explicit[ec.Name] = ec
		}
	}

	// Determine which events to index.
	var eventsToIndex []*abi.EventDef

	if wildcard != nil {
		// Wildcard: all ABI events.
		eventsToIndex = registry.Events()
	} else {
		// Only explicitly listed events.
		for name := range explicit {
			if ev := registry.MatchName(name); ev != nil {
				eventsToIndex = append(eventsToIndex, ev)
			}
		}
	}

	for _, ev := range eventsToIndex {
		// Use explicit config if available, otherwise wildcard default.
		var ec config.EventConfig
		if explicitEC, ok := explicit[ev.Name]; ok {
			ec = explicitEC
		} else if wildcard != nil {
			ec = *wildcard
			ec.Name = ev.Name
		} else {
			continue
		}

		schema := BuildTableSchema(cc.Name, ev, ec, opts)
		schemas[ev.Name] = schema
	}

	return schemas
}

// BuildTableSchema creates a single TableSchema from an event definition and config.
// Pass opts for factory shared tables; nil for normal contracts.
func BuildTableSchema(contractName string, ev *abi.EventDef, ec config.EventConfig, opts *BuildOptions) *types.TableSchema {
	tableName := strings.ToLower(contractName + "_" + ev.Name)
	shared := false
	contract := contractName

	if opts != nil && opts.SharedTable {
		tableName = strings.ToLower(opts.FactoryName + "_" + ev.Name)
		contract = opts.FactoryName
		shared = true
	}

	// Start with standard metadata columns.
	columns := MetadataColumns()

	// Add contract_name column for shared tables.
	if shared {
		columns = append(columns, types.Column{Name: "contract_name", Type: "string"})
	}

	// Event-specific columns from ABI.
	for _, member := range ev.KeyMembers {
		columns = append(columns, types.Column{
			Name: member.Name,
			Type: CairoTypeToColumnType(member.Type),
		})
	}
	for _, member := range ev.DataMembers {
		columns = append(columns, types.Column{
			Name: member.Name,
			Type: CairoTypeToColumnType(member.Type),
		})
	}

	// Determine table type.
	tableType := types.TableTypeLog
	switch ec.Table.Type {
	case "unique":
		tableType = types.TableTypeUnique
	case "aggregation":
		tableType = types.TableTypeAggregation
	}

	// Build aggregate specs.
	var aggregates []types.AggregateSpec
	for _, agg := range ec.Table.Aggregates {
		aggregates = append(aggregates, types.AggregateSpec{
			Column:    agg.Column,
			Operation: agg.Operation,
			Field:     agg.Field,
		})
	}

	return &types.TableSchema{
		Name:        tableName,
		Contract:    contract,
		Event:       ev.Name,
		TableType:   tableType,
		Columns:     columns,
		UniqueKey:   ec.Table.UniqueKey,
		Aggregates:  aggregates,
		SharedTable: shared,
	}
}

// ViewMetadataColumns returns the standard metadata columns added to view function tables.
func ViewMetadataColumns() []types.Column {
	return []types.Column{
		{Name: "block_number", Type: "uint64"},
		{Name: "timestamp", Type: "uint64"},
		{Name: "contract_address", Type: "string"},
		{Name: "_view_key", Type: "string"},
	}
}

// BuildViewSchema creates a TableSchema for a view function's polled results.
// Maps function output members to table columns using the same type mapping as events.
// Pass opts for shared view tables (e.g., discovered contracts with shared_tables: true); nil for normal contracts.
func BuildViewSchema(contractName string, funcDef *abi.FunctionDef, viewCfg *config.ViewConfig, opts *BuildOptions) *types.TableSchema {
	tableName := strings.ToLower(contractName + "_" + funcDef.Name)
	contract := contractName
	shared := false

	if opts != nil && opts.SharedTable {
		tableName = strings.ToLower(opts.FactoryName + "_" + funcDef.Name)
		contract = opts.FactoryName
		shared = true
	}

	columns := ViewMetadataColumns()

	// Add contract_name column for shared view tables.
	if shared {
		columns = append(columns, types.Column{Name: "contract_name", Type: "string"})
	}

	// Add output columns from function definition.
	for i, output := range funcDef.Outputs {
		name := output.Name
		if name == "" {
			if len(funcDef.Outputs) == 1 {
				name = funcDef.Name
			} else {
				name = fmt.Sprintf("output_%d", i)
			}
		}
		columns = append(columns, types.Column{
			Name: name,
			Type: CairoTypeToColumnType(output.Type),
		})
	}

	tableType := types.TableTypeLog
	if viewCfg.Table.Type == "unique" {
		tableType = types.TableTypeUnique
	}

	return &types.TableSchema{
		Name:        tableName,
		Contract:    contract,
		Event:       funcDef.Name,
		TableType:   tableType,
		Columns:     columns,
		UniqueKey:   viewCfg.Table.UniqueKey,
		SharedTable: shared,
	}
}

// CairoTypeToColumnType maps a Cairo type definition to a store column type string.
func CairoTypeToColumnType(td *abi.TypeDef) string {
	switch td.Kind {
	case abi.CairoFelt252, abi.CairoContractAddress, abi.CairoClassHash:
		return "string"
	case abi.CairoU8, abi.CairoU16, abi.CairoU32, abi.CairoU64:
		return "int64"
	case abi.CairoU128, abi.CairoU256:
		return "string" // Too large for int64.
	case abi.CairoI8, abi.CairoI16, abi.CairoI32, abi.CairoI64:
		return "int64"
	case abi.CairoI128:
		return "string"
	case abi.CairoBool:
		return "bool"
	case abi.CairoByteArray:
		return "string"
	case abi.CairoArray, abi.CairoSpan:
		return "string" // JSON-encoded.
	case abi.CairoStruct, abi.CairoTuple:
		return "string" // JSON-encoded.
	case abi.CairoEnum:
		return "string" // JSON-encoded.
	default:
		return "string"
	}
}
