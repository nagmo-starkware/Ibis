package schema

import (
	"strings"
	"testing"

	"github.com/b-j-roberts/ibis/internal/abi"
	"github.com/b-j-roberts/ibis/internal/config"
	"github.com/b-j-roberts/ibis/internal/types"
)

// --- Helpers ---

func testEventDef(name string, keyMembers, dataMembers []abi.FieldDef) *abi.EventDef {
	return &abi.EventDef{
		Name:        name,
		FullName:    "test::" + name,
		Selector:    abi.ComputeSelector(name),
		KeyMembers:  keyMembers,
		DataMembers: dataMembers,
	}
}

func simpleEventDef(name string) *abi.EventDef {
	return testEventDef(name,
		[]abi.FieldDef{{Name: "sender", Type: &abi.TypeDef{Kind: abi.CairoContractAddress, Name: "ContractAddress"}}},
		[]abi.FieldDef{{Name: "amount", Type: &abi.TypeDef{Kind: abi.CairoU64, Name: "u64"}}},
	)
}

func makeRegistry(events []*abi.EventDef) *abi.EventRegistry {
	parsedABI := &abi.ABI{
		Types:  make(map[string]*abi.TypeDef),
		Events: events,
	}
	return abi.NewEventRegistry(parsedABI)
}

// --- BuildSchemas Tests ---

func TestBuildSchemas_ExplicitEvents(t *testing.T) {
	transferDef := simpleEventDef("Transfer")
	approvalDef := simpleEventDef("Approval")
	registry := makeRegistry([]*abi.EventDef{transferDef, approvalDef})

	cc := config.ContractConfig{
		Name:    "mytoken",
		Address: "0x123",
		Events: []config.EventConfig{
			{Name: "Transfer", Table: config.TableConfig{Type: "log"}},
		},
	}

	schemas := BuildSchemas(&cc, &abi.ABI{Events: []*abi.EventDef{transferDef, approvalDef}}, registry, nil)

	if len(schemas) != 1 {
		t.Fatalf("expected 1 schema, got %d", len(schemas))
	}
	if _, ok := schemas["Transfer"]; !ok {
		t.Fatal("expected Transfer schema")
	}
	if schemas["Transfer"].TableType != types.TableTypeLog {
		t.Fatal("expected log table type")
	}
	// Approval should not be present.
	if _, ok := schemas["Approval"]; ok {
		t.Fatal("Approval should not be in schemas without wildcard")
	}
}

func TestBuildSchemas_Wildcard(t *testing.T) {
	transferDef := simpleEventDef("Transfer")
	approvalDef := simpleEventDef("Approval")
	mintDef := simpleEventDef("Mint")
	registry := makeRegistry([]*abi.EventDef{transferDef, approvalDef, mintDef})

	cc := config.ContractConfig{
		Name:    "mytoken",
		Address: "0x123",
		Events: []config.EventConfig{
			{Name: "*", Table: config.TableConfig{Type: "log"}},
		},
	}

	schemas := BuildSchemas(&cc, &abi.ABI{Events: []*abi.EventDef{transferDef, approvalDef, mintDef}}, registry, nil)

	if len(schemas) != 3 {
		t.Fatalf("expected 3 schemas (wildcard), got %d", len(schemas))
	}
	for _, name := range []string{"Transfer", "Approval", "Mint"} {
		if _, ok := schemas[name]; !ok {
			t.Fatalf("expected %s schema", name)
		}
		if schemas[name].TableType != types.TableTypeLog {
			t.Fatalf("expected %s to be log type", name)
		}
	}
}

func TestBuildSchemas_WildcardWithOverride(t *testing.T) {
	transferDef := simpleEventDef("Transfer")
	approvalDef := simpleEventDef("Approval")
	registry := makeRegistry([]*abi.EventDef{transferDef, approvalDef})

	cc := config.ContractConfig{
		Name:    "mytoken",
		Address: "0x123",
		Events: []config.EventConfig{
			{Name: "*", Table: config.TableConfig{Type: "log"}},
			{Name: "Transfer", Table: config.TableConfig{Type: "unique", UniqueKey: "sender"}},
		},
	}

	schemas := BuildSchemas(&cc, &abi.ABI{Events: []*abi.EventDef{transferDef, approvalDef}}, registry, nil)

	if len(schemas) != 2 {
		t.Fatalf("expected 2 schemas, got %d", len(schemas))
	}

	// Transfer should be overridden to unique.
	if schemas["Transfer"].TableType != types.TableTypeUnique {
		t.Fatal("expected Transfer to be unique type (override)")
	}
	if schemas["Transfer"].UniqueKey != "sender" {
		t.Fatal("expected Transfer unique key to be 'sender'")
	}

	// Approval should use wildcard default (log).
	if schemas["Approval"].TableType != types.TableTypeLog {
		t.Fatal("expected Approval to be log type (wildcard default)")
	}
}

func TestBuildSchemas_WildcardWithAggregationOverride(t *testing.T) {
	transferDef := simpleEventDef("Transfer")
	volumeDef := simpleEventDef("VolumeUpdate")
	registry := makeRegistry([]*abi.EventDef{transferDef, volumeDef})

	cc := config.ContractConfig{
		Name:    "dex",
		Address: "0x456",
		Events: []config.EventConfig{
			{Name: "*", Table: config.TableConfig{Type: "log"}},
			{Name: "VolumeUpdate", Table: config.TableConfig{
				Type: "aggregation",
				Aggregates: []config.AggregateConfig{
					{Column: "total_volume", Operation: "sum", Field: "amount"},
					{Column: "trade_count", Operation: "count", Field: "amount"},
				},
			}},
		},
	}

	schemas := BuildSchemas(&cc, &abi.ABI{Events: []*abi.EventDef{transferDef, volumeDef}}, registry, nil)

	if schemas["VolumeUpdate"].TableType != types.TableTypeAggregation {
		t.Fatal("expected VolumeUpdate to be aggregation type")
	}
	if len(schemas["VolumeUpdate"].Aggregates) != 2 {
		t.Fatalf("expected 2 aggregate specs, got %d", len(schemas["VolumeUpdate"].Aggregates))
	}
	if schemas["VolumeUpdate"].Aggregates[0].Column != "total_volume" {
		t.Fatal("expected first aggregate column to be total_volume")
	}
	if schemas["VolumeUpdate"].Aggregates[1].Operation != "count" {
		t.Fatal("expected second aggregate operation to be count")
	}

	// Transfer should remain log.
	if schemas["Transfer"].TableType != types.TableTypeLog {
		t.Fatal("expected Transfer to be log type")
	}
}

func TestBuildSchemas_NoEventsConfigured(t *testing.T) {
	transferDef := simpleEventDef("Transfer")
	registry := makeRegistry([]*abi.EventDef{transferDef})

	cc := config.ContractConfig{
		Name:    "mytoken",
		Address: "0x123",
		Events:  []config.EventConfig{},
	}

	schemas := BuildSchemas(&cc, &abi.ABI{Events: []*abi.EventDef{transferDef}}, registry, nil)

	if len(schemas) != 0 {
		t.Fatalf("expected 0 schemas with no events configured, got %d", len(schemas))
	}
}

func TestBuildSchemas_NonexistentEventIgnored(t *testing.T) {
	transferDef := simpleEventDef("Transfer")
	registry := makeRegistry([]*abi.EventDef{transferDef})

	cc := config.ContractConfig{
		Name:    "mytoken",
		Address: "0x123",
		Events: []config.EventConfig{
			{Name: "NonExistentEvent", Table: config.TableConfig{Type: "log"}},
		},
	}

	schemas := BuildSchemas(&cc, &abi.ABI{Events: []*abi.EventDef{transferDef}}, registry, nil)

	if len(schemas) != 0 {
		t.Fatalf("expected 0 schemas for nonexistent event, got %d", len(schemas))
	}
}

// --- BuildTableSchema Tests ---

func TestBuildTableSchema_TableName(t *testing.T) {
	ev := simpleEventDef("Transfer")
	ec := config.EventConfig{Name: "Transfer", Table: config.TableConfig{Type: "log"}}

	schema := BuildTableSchema("MyToken", ev, ec, nil)

	if schema.Name != "mytoken_transfer" {
		t.Fatalf("expected lowercase table name 'mytoken_transfer', got '%s'", schema.Name)
	}
	if schema.Contract != "MyToken" {
		t.Fatalf("expected contract 'MyToken', got '%s'", schema.Contract)
	}
	if schema.Event != "Transfer" {
		t.Fatalf("expected event 'Transfer', got '%s'", schema.Event)
	}
}

func TestBuildTableSchema_MetadataColumns(t *testing.T) {
	ev := simpleEventDef("Transfer")
	ec := config.EventConfig{Name: "Transfer", Table: config.TableConfig{Type: "log"}}

	schema := BuildTableSchema("mytoken", ev, ec, nil)

	colNames := make(map[string]string)
	for _, col := range schema.Columns {
		colNames[col.Name] = col.Type
	}

	expectedMeta := map[string]string{
		"block_number":     "uint64",
		"transaction_hash": "string",
		"log_index":        "uint64",
		"timestamp":        "uint64",
		"contract_address": "string",
		"event_name":       "string",
		"status":           "string",
	}

	for name, typ := range expectedMeta {
		if colNames[name] != typ {
			t.Errorf("metadata column %s: expected type %s, got %s", name, typ, colNames[name])
		}
	}
}

func TestBuildTableSchema_EventColumns(t *testing.T) {
	ev := testEventDef("Transfer",
		[]abi.FieldDef{
			{Name: "from", Type: &abi.TypeDef{Kind: abi.CairoContractAddress}},
			{Name: "to", Type: &abi.TypeDef{Kind: abi.CairoContractAddress}},
		},
		[]abi.FieldDef{
			{Name: "amount", Type: &abi.TypeDef{Kind: abi.CairoU256}},
			{Name: "is_mint", Type: &abi.TypeDef{Kind: abi.CairoBool}},
		},
	)
	ec := config.EventConfig{Name: "Transfer", Table: config.TableConfig{Type: "log"}}

	schema := BuildTableSchema("token", ev, ec, nil)

	colMap := make(map[string]string)
	for _, col := range schema.Columns {
		colMap[col.Name] = col.Type
	}

	// Key members.
	if colMap["from"] != "string" {
		t.Errorf("expected 'from' as string, got %s", colMap["from"])
	}
	if colMap["to"] != "string" {
		t.Errorf("expected 'to' as string, got %s", colMap["to"])
	}
	// Data members.
	if colMap["amount"] != "string" {
		t.Errorf("expected 'amount' (u256) as string, got %s", colMap["amount"])
	}
	if colMap["is_mint"] != "bool" {
		t.Errorf("expected 'is_mint' as bool, got %s", colMap["is_mint"])
	}
}

func TestBuildTableSchema_UniqueTable(t *testing.T) {
	ev := simpleEventDef("LeaderboardUpdate")
	ec := config.EventConfig{
		Name: "LeaderboardUpdate",
		Table: config.TableConfig{
			Type:      "unique",
			UniqueKey: "sender",
		},
	}

	schema := BuildTableSchema("game", ev, ec, nil)

	if schema.TableType != types.TableTypeUnique {
		t.Fatalf("expected unique table type, got %v", schema.TableType)
	}
	if schema.UniqueKey != "sender" {
		t.Fatalf("expected unique key 'sender', got '%s'", schema.UniqueKey)
	}
}

func TestBuildTableSchema_AggregationTable(t *testing.T) {
	ev := simpleEventDef("VolumeUpdate")
	ec := config.EventConfig{
		Name: "VolumeUpdate",
		Table: config.TableConfig{
			Type: "aggregation",
			Aggregates: []config.AggregateConfig{
				{Column: "total_volume", Operation: "sum", Field: "amount"},
				{Column: "trade_count", Operation: "count", Field: "amount"},
				{Column: "avg_trade", Operation: "avg", Field: "amount"},
			},
		},
	}

	schema := BuildTableSchema("dex", ev, ec, nil)

	if schema.TableType != types.TableTypeAggregation {
		t.Fatalf("expected aggregation table type, got %v", schema.TableType)
	}
	if len(schema.Aggregates) != 3 {
		t.Fatalf("expected 3 aggregate specs, got %d", len(schema.Aggregates))
	}

	aggs := schema.Aggregates
	if aggs[0].Column != "total_volume" || aggs[0].Operation != "sum" || aggs[0].Field != "amount" {
		t.Errorf("unexpected first aggregate: %+v", aggs[0])
	}
	if aggs[1].Column != "trade_count" || aggs[1].Operation != "count" {
		t.Errorf("unexpected second aggregate: %+v", aggs[1])
	}
	if aggs[2].Column != "avg_trade" || aggs[2].Operation != "avg" {
		t.Errorf("unexpected third aggregate: %+v", aggs[2])
	}
}

// --- CairoTypeToColumnType Tests ---

func TestCairoTypeToColumnType(t *testing.T) {
	tests := []struct {
		name     string
		kind     abi.CairoType
		expected string
	}{
		{"felt252", abi.CairoFelt252, "string"},
		{"ContractAddress", abi.CairoContractAddress, "string"},
		{"ClassHash", abi.CairoClassHash, "string"},
		{"u8", abi.CairoU8, "int64"},
		{"u16", abi.CairoU16, "int64"},
		{"u32", abi.CairoU32, "int64"},
		{"u64", abi.CairoU64, "int64"},
		{"u128", abi.CairoU128, "string"},
		{"u256", abi.CairoU256, "string"},
		{"i8", abi.CairoI8, "int64"},
		{"i16", abi.CairoI16, "int64"},
		{"i32", abi.CairoI32, "int64"},
		{"i64", abi.CairoI64, "int64"},
		{"i128", abi.CairoI128, "string"},
		{"bool", abi.CairoBool, "bool"},
		{"ByteArray", abi.CairoByteArray, "string"},
		{"Array", abi.CairoArray, "string"},
		{"Span", abi.CairoSpan, "string"},
		{"Struct", abi.CairoStruct, "string"},
		{"Enum", abi.CairoEnum, "string"},
		{"Unit", abi.CairoUnit, "string"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			td := &abi.TypeDef{Kind: tt.kind}
			result := CairoTypeToColumnType(td)
			if result != tt.expected {
				t.Errorf("CairoTypeToColumnType(%s) = %s, want %s", tt.name, result, tt.expected)
			}
		})
	}
}

// --- Postgres SQL Generation Tests ---

func TestGenerateCreateTableSQL_LogTable(t *testing.T) {
	schema := &types.TableSchema{
		Name:      "mytoken_transfer",
		Contract:  "mytoken",
		Event:     "Transfer",
		TableType: types.TableTypeLog,
		Columns: []types.Column{
			{Name: "block_number", Type: "uint64"},
			{Name: "transaction_hash", Type: "string"},
			{Name: "log_index", Type: "uint64"},
			{Name: "timestamp", Type: "uint64"},
			{Name: "sender", Type: "string"},
			{Name: "amount", Type: "int64"},
			{Name: "is_active", Type: "bool"},
		},
	}

	sql := GenerateCreateTableSQL(schema)

	// Should contain CREATE TABLE.
	if !strings.Contains(sql, "CREATE TABLE IF NOT EXISTS mytoken_transfer") {
		t.Fatal("expected CREATE TABLE statement")
	}

	// Check column types.
	if !strings.Contains(sql, "block_number BIGINT") {
		t.Fatal("expected block_number BIGINT")
	}
	if !strings.Contains(sql, "transaction_hash TEXT") {
		t.Fatal("expected transaction_hash TEXT")
	}
	if !strings.Contains(sql, "amount BIGINT") {
		t.Fatal("expected amount BIGINT")
	}
	if !strings.Contains(sql, "is_active BOOLEAN") {
		t.Fatal("expected is_active BOOLEAN")
	}

	// Standard indices.
	if !strings.Contains(sql, "CREATE INDEX IF NOT EXISTS idx_mytoken_transfer_block ON") {
		t.Fatal("expected block index")
	}
	if !strings.Contains(sql, "CREATE INDEX IF NOT EXISTS idx_mytoken_transfer_block_log ON") {
		t.Fatal("expected block_log composite index")
	}
	if !strings.Contains(sql, "CREATE INDEX IF NOT EXISTS idx_mytoken_transfer_status ON") {
		t.Fatal("expected status index")
	}

	// Should NOT have unique index for log table.
	if strings.Contains(sql, "UNIQUE INDEX") {
		t.Fatal("log table should not have unique index")
	}
}

func TestGenerateCreateTableSQL_UniqueTable(t *testing.T) {
	schema := &types.TableSchema{
		Name:      "game_leaderboard",
		Contract:  "game",
		Event:     "LeaderboardUpdate",
		TableType: types.TableTypeUnique,
		UniqueKey: "player_address",
		Columns: []types.Column{
			{Name: "block_number", Type: "uint64"},
			{Name: "player_address", Type: "string"},
			{Name: "score", Type: "int64"},
		},
	}

	sql := GenerateCreateTableSQL(schema)

	if !strings.Contains(sql, "CREATE UNIQUE INDEX IF NOT EXISTS idx_game_leaderboard_unique_player_address ON game_leaderboard (player_address)") {
		t.Fatal("expected unique index on player_address")
	}
}

func TestGenerateAggregationTableSQL(t *testing.T) {
	schema := &types.TableSchema{
		Name:      "dex_volume",
		Contract:  "dex",
		Event:     "VolumeUpdate",
		TableType: types.TableTypeAggregation,
		Aggregates: []types.AggregateSpec{
			{Column: "total_volume", Operation: "sum", Field: "amount"},
			{Column: "trade_count", Operation: "count", Field: "amount"},
		},
	}

	sql := GenerateAggregationTableSQL(schema)

	if !strings.Contains(sql, "CREATE TABLE IF NOT EXISTS dex_volume_agg") {
		t.Fatal("expected aggregation table")
	}
	if !strings.Contains(sql, "total_volume DOUBLE PRECISION") {
		t.Fatal("expected total_volume as DOUBLE PRECISION")
	}
	if !strings.Contains(sql, "trade_count BIGINT") {
		t.Fatal("expected trade_count as BIGINT for count operation")
	}
}

func TestGenerateAggregationTableSQL_NonAggTable(t *testing.T) {
	schema := &types.TableSchema{
		Name:      "mytoken_transfer",
		TableType: types.TableTypeLog,
	}

	sql := GenerateAggregationTableSQL(schema)
	if sql != "" {
		t.Fatalf("expected empty SQL for non-aggregation table, got %s", sql)
	}
}

// --- BadgerDB Key Pattern Tests ---

func TestGenerateBadgerKeyPatterns_LogTable(t *testing.T) {
	schema := &types.TableSchema{
		Name:      "mytoken_transfer",
		TableType: types.TableTypeLog,
	}

	patterns := GenerateBadgerKeyPatterns(schema)

	if patterns.PrimaryPrefix != "evt:mytoken_transfer:" {
		t.Fatalf("expected primary prefix 'evt:mytoken_transfer:', got '%s'", patterns.PrimaryPrefix)
	}
	if patterns.ReversePrefix != "rev:mytoken_transfer:" {
		t.Fatalf("expected reverse prefix 'rev:mytoken_transfer:', got '%s'", patterns.ReversePrefix)
	}
	if patterns.SchemaKey != "schema:mytoken_transfer" {
		t.Fatalf("expected schema key 'schema:mytoken_transfer', got '%s'", patterns.SchemaKey)
	}
	if patterns.UniquePrefix != "" {
		t.Fatal("log table should not have unique prefix")
	}
	if patterns.AggregationKey != "" {
		t.Fatal("log table should not have aggregation key")
	}
}

func TestGenerateBadgerKeyPatterns_UniqueTable(t *testing.T) {
	schema := &types.TableSchema{
		Name:      "game_leaderboard",
		TableType: types.TableTypeUnique,
		UniqueKey: "player",
	}

	patterns := GenerateBadgerKeyPatterns(schema)

	if patterns.UniquePrefix != "unq:game_leaderboard:" {
		t.Fatalf("expected unique prefix 'unq:game_leaderboard:', got '%s'", patterns.UniquePrefix)
	}
}

func TestGenerateBadgerKeyPatterns_AggregationTable(t *testing.T) {
	schema := &types.TableSchema{
		Name:      "dex_volume",
		TableType: types.TableTypeAggregation,
	}

	patterns := GenerateBadgerKeyPatterns(schema)

	if patterns.AggregationKey != "agg:dex_volume" {
		t.Fatalf("expected aggregation key 'agg:dex_volume', got '%s'", patterns.AggregationKey)
	}
}

// --- BuildViewSchema Tests ---

func TestBuildViewSchema_BasicFunction(t *testing.T) {
	funcDef := &abi.FunctionDef{
		Name:     "total_supply",
		FullName: "test::total_supply",
		Selector: abi.ComputeSelector("total_supply"),
		Outputs:  []abi.FieldDef{{Name: "value", Type: &abi.TypeDef{Kind: abi.CairoU256, Name: "u256"}}},
	}
	viewCfg := config.ViewConfig{
		Function: "total_supply",
		Interval: "30s",
		Table: config.TableConfig{
			Type:      "unique",
			UniqueKey: "_view_key",
		},
	}

	schema := BuildViewSchema("MyToken", funcDef, viewCfg)

	if schema.Name != "mytoken_total_supply" {
		t.Fatalf("expected table name 'mytoken_total_supply', got '%s'", schema.Name)
	}
	if schema.Contract != "MyToken" {
		t.Fatalf("expected contract 'MyToken', got '%s'", schema.Contract)
	}
	if schema.Event != "total_supply" {
		t.Fatalf("expected event 'total_supply', got '%s'", schema.Event)
	}
	if schema.TableType != types.TableTypeUnique {
		t.Fatal("expected unique table type")
	}
	if schema.UniqueKey != "_view_key" {
		t.Fatalf("expected unique key '_view_key', got '%s'", schema.UniqueKey)
	}
}

func TestBuildViewSchema_LogType(t *testing.T) {
	funcDef := &abi.FunctionDef{
		Name:    "get_price",
		Outputs: []abi.FieldDef{{Name: "price", Type: &abi.TypeDef{Kind: abi.CairoU128}}},
	}
	viewCfg := config.ViewConfig{
		Function: "get_price",
		Interval: "5m",
		Table:    config.TableConfig{Type: "log"},
	}

	schema := BuildViewSchema("Oracle", funcDef, viewCfg)
	if schema.TableType != types.TableTypeLog {
		t.Fatal("expected log table type")
	}
}

func TestBuildViewSchema_Columns(t *testing.T) {
	funcDef := &abi.FunctionDef{
		Name: "get_price",
		Outputs: []abi.FieldDef{
			{Name: "price", Type: &abi.TypeDef{Kind: abi.CairoU128}},
			{Name: "decimals", Type: &abi.TypeDef{Kind: abi.CairoU8}},
			{Name: "is_valid", Type: &abi.TypeDef{Kind: abi.CairoBool}},
		},
	}
	viewCfg := config.ViewConfig{
		Function: "get_price",
		Interval: "30s",
		Table:    config.TableConfig{Type: "log"},
	}

	schema := BuildViewSchema("Oracle", funcDef, viewCfg)

	colMap := make(map[string]string)
	for _, col := range schema.Columns {
		colMap[col.Name] = col.Type
	}

	// View metadata columns.
	if colMap["block_number"] != "uint64" {
		t.Error("missing block_number uint64")
	}
	if colMap["timestamp"] != "uint64" {
		t.Error("missing timestamp uint64")
	}
	if colMap["contract_address"] != "string" {
		t.Error("missing contract_address string")
	}
	if colMap["_view_key"] != "string" {
		t.Error("missing _view_key string")
	}

	// Output columns.
	if colMap["price"] != "string" { // u128 -> string
		t.Errorf("expected price as string, got %s", colMap["price"])
	}
	if colMap["decimals"] != "int64" { // u8 -> int64
		t.Errorf("expected decimals as int64, got %s", colMap["decimals"])
	}
	if colMap["is_valid"] != "bool" {
		t.Errorf("expected is_valid as bool, got %s", colMap["is_valid"])
	}

	// Should NOT have event-only metadata columns.
	for _, name := range []string{"log_index", "transaction_hash", "event_name", "status"} {
		if _, ok := colMap[name]; ok {
			t.Errorf("view table should not have %s column", name)
		}
	}
}

func TestViewMetadataColumns(t *testing.T) {
	cols := ViewMetadataColumns()
	expected := []string{"block_number", "timestamp", "contract_address", "_view_key"}
	if len(cols) != len(expected) {
		t.Fatalf("expected %d view metadata columns, got %d", len(expected), len(cols))
	}
	for i, col := range cols {
		if col.Name != expected[i] {
			t.Errorf("column %d: expected %s, got %s", i, expected[i], col.Name)
		}
	}
}

// --- MetadataColumns Tests ---

func TestMetadataColumns(t *testing.T) {
	cols := MetadataColumns()

	expected := []string{"block_number", "transaction_hash", "log_index", "timestamp", "contract_address", "event_name", "status"}

	if len(cols) != len(expected) {
		t.Fatalf("expected %d metadata columns, got %d", len(expected), len(cols))
	}

	for i, col := range cols {
		if col.Name != expected[i] {
			t.Errorf("metadata column %d: expected %s, got %s", i, expected[i], col.Name)
		}
	}
}

// --- Integration: Full schema generation pipeline ---

func TestFullPipeline_WildcardWithMixedTypes(t *testing.T) {
	// Simulate a real contract with multiple event types.
	transfer := testEventDef("Transfer",
		[]abi.FieldDef{{Name: "from", Type: &abi.TypeDef{Kind: abi.CairoContractAddress}}},
		[]abi.FieldDef{
			{Name: "to", Type: &abi.TypeDef{Kind: abi.CairoContractAddress}},
			{Name: "value", Type: &abi.TypeDef{Kind: abi.CairoU256}},
		},
	)
	approval := testEventDef("Approval",
		[]abi.FieldDef{{Name: "owner", Type: &abi.TypeDef{Kind: abi.CairoContractAddress}}},
		[]abi.FieldDef{
			{Name: "spender", Type: &abi.TypeDef{Kind: abi.CairoContractAddress}},
			{Name: "value", Type: &abi.TypeDef{Kind: abi.CairoU256}},
		},
	)
	leaderboard := testEventDef("LeaderboardUpdate",
		[]abi.FieldDef{{Name: "trader", Type: &abi.TypeDef{Kind: abi.CairoContractAddress}}},
		[]abi.FieldDef{{Name: "score", Type: &abi.TypeDef{Kind: abi.CairoU64}}},
	)
	volume := testEventDef("VolumeUpdate",
		[]abi.FieldDef{},
		[]abi.FieldDef{{Name: "amount", Type: &abi.TypeDef{Kind: abi.CairoU128}}},
	)

	events := []*abi.EventDef{transfer, approval, leaderboard, volume}
	registry := makeRegistry(events)
	contractABI := &abi.ABI{Types: make(map[string]*abi.TypeDef), Events: events}

	cc := config.ContractConfig{
		Name:    "StarknetOptions",
		Address: "0x049d36570d4e46",
		Events: []config.EventConfig{
			{Name: "*", Table: config.TableConfig{Type: "log"}},
			{Name: "LeaderboardUpdate", Table: config.TableConfig{Type: "unique", UniqueKey: "trader"}},
			{Name: "VolumeUpdate", Table: config.TableConfig{
				Type: "aggregation",
				Aggregates: []config.AggregateConfig{
					{Column: "total_volume", Operation: "sum", Field: "amount"},
					{Column: "trade_count", Operation: "count", Field: "amount"},
				},
			}},
		},
	}

	schemas := BuildSchemas(&cc, contractABI, registry, nil)

	// All 4 events should have schemas.
	if len(schemas) != 4 {
		t.Fatalf("expected 4 schemas, got %d", len(schemas))
	}

	// Transfer = log (wildcard default).
	if schemas["Transfer"].TableType != types.TableTypeLog {
		t.Fatal("Transfer should be log")
	}
	if schemas["Transfer"].Name != "starknetoptions_transfer" {
		t.Fatalf("expected table name 'starknetoptions_transfer', got '%s'", schemas["Transfer"].Name)
	}

	// Approval = log (wildcard default).
	if schemas["Approval"].TableType != types.TableTypeLog {
		t.Fatal("Approval should be log")
	}

	// LeaderboardUpdate = unique (override).
	if schemas["LeaderboardUpdate"].TableType != types.TableTypeUnique {
		t.Fatal("LeaderboardUpdate should be unique")
	}
	if schemas["LeaderboardUpdate"].UniqueKey != "trader" {
		t.Fatal("LeaderboardUpdate unique key should be 'trader'")
	}

	// VolumeUpdate = aggregation (override).
	if schemas["VolumeUpdate"].TableType != types.TableTypeAggregation {
		t.Fatal("VolumeUpdate should be aggregation")
	}
	if len(schemas["VolumeUpdate"].Aggregates) != 2 {
		t.Fatalf("expected 2 aggregates for VolumeUpdate, got %d", len(schemas["VolumeUpdate"].Aggregates))
	}

	// Verify Postgres SQL can be generated for each.
	for name, s := range schemas {
		sql := GenerateCreateTableSQL(s)
		if sql == "" {
			t.Fatalf("empty SQL for schema %s", name)
		}
		if !strings.Contains(sql, "CREATE TABLE IF NOT EXISTS") {
			t.Fatalf("SQL for %s missing CREATE TABLE", name)
		}
	}

	// Verify BadgerDB patterns for each.
	for name, s := range schemas {
		patterns := GenerateBadgerKeyPatterns(s)
		if patterns.PrimaryPrefix == "" {
			t.Fatalf("empty primary prefix for schema %s", name)
		}
	}
}
