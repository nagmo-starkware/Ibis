package abi

import (
	"math/big"
	"testing"

	"github.com/NethermindEth/juno/core/felt"
	"github.com/NethermindEth/starknet.go/utils"
)

// --- Test ABI JSON fixtures ---

// Minimal ERC-20-like ABI with Transfer event, u256 struct, and bool enum.
const erc20ABI = `[
	{
		"type": "struct",
		"name": "core::integer::u256",
		"members": [
			{"name": "low", "type": "core::integer::u128"},
			{"name": "high", "type": "core::integer::u128"}
		]
	},
	{
		"type": "enum",
		"name": "core::bool",
		"variants": [
			{"name": "False", "type": "()"},
			{"name": "True", "type": "()"}
		]
	},
	{
		"type": "event",
		"name": "openzeppelin::token::erc20::erc20::ERC20Component::Transfer",
		"kind": "struct",
		"members": [
			{"name": "from", "type": "core::starknet::contract_address::ContractAddress", "kind": "key"},
			{"name": "to", "type": "core::starknet::contract_address::ContractAddress", "kind": "key"},
			{"name": "value", "type": "core::integer::u256", "kind": "data"}
		]
	},
	{
		"type": "event",
		"name": "openzeppelin::token::erc20::erc20::ERC20Component::Approval",
		"kind": "struct",
		"members": [
			{"name": "owner", "type": "core::starknet::contract_address::ContractAddress", "kind": "key"},
			{"name": "spender", "type": "core::starknet::contract_address::ContractAddress", "kind": "key"},
			{"name": "value", "type": "core::integer::u256", "kind": "data"}
		]
	},
	{
		"type": "event",
		"name": "openzeppelin::token::erc20::erc20::ERC20Component::Event",
		"kind": "enum",
		"variants": [
			{"name": "Transfer", "type": "openzeppelin::token::erc20::erc20::ERC20Component::Transfer", "kind": "flat"},
			{"name": "Approval", "type": "openzeppelin::token::erc20::erc20::ERC20Component::Approval", "kind": "flat"}
		]
	}
]`

// ABI with various primitive types, arrays, and ByteArray.
const complexABI = `[
	{
		"type": "struct",
		"name": "mycontract::GameState",
		"members": [
			{"name": "player", "type": "core::starknet::contract_address::ContractAddress"},
			{"name": "score", "type": "core::integer::u64"},
			{"name": "active", "type": "core::bool"}
		]
	},
	{
		"type": "enum",
		"name": "core::bool",
		"variants": [
			{"name": "False", "type": "()"},
			{"name": "True", "type": "()"}
		]
	},
	{
		"type": "event",
		"name": "mycontract::ScoreUpdated",
		"kind": "struct",
		"members": [
			{"name": "player", "type": "core::starknet::contract_address::ContractAddress", "kind": "key"},
			{"name": "old_score", "type": "core::integer::u64", "kind": "data"},
			{"name": "new_score", "type": "core::integer::u64", "kind": "data"},
			{"name": "is_highscore", "type": "core::bool", "kind": "data"}
		]
	},
	{
		"type": "event",
		"name": "mycontract::MessagePosted",
		"kind": "struct",
		"members": [
			{"name": "sender", "type": "core::starknet::contract_address::ContractAddress", "kind": "key"},
			{"name": "message", "type": "core::byte_array::ByteArray", "kind": "data"}
		]
	},
	{
		"type": "event",
		"name": "mycontract::BatchTransfer",
		"kind": "struct",
		"members": [
			{"name": "sender", "type": "core::starknet::contract_address::ContractAddress", "kind": "key"},
			{"name": "amounts", "type": "core::array::Array::<core::integer::u64>", "kind": "data"}
		]
	},
	{
		"type": "event",
		"name": "mycontract::SignedEvent",
		"kind": "struct",
		"members": [
			{"name": "value_i8", "type": "core::integer::i8", "kind": "data"},
			{"name": "value_i32", "type": "core::integer::i32", "kind": "data"},
			{"name": "value_i128", "type": "core::integer::i128", "kind": "data"}
		]
	},
	{
		"type": "event",
		"name": "mycontract::StructEvent",
		"kind": "struct",
		"members": [
			{"name": "state", "type": "mycontract::GameState", "kind": "data"}
		]
	}
]`

// Contract class JSON format (ABI as embedded string).
const contractClassJSON = `{
	"sierra_program": [],
	"contract_class_version": "0.1.0",
	"entry_points_by_type": {},
	"abi": "[{\"type\":\"event\",\"name\":\"contracts::SimpleEvent\",\"kind\":\"struct\",\"members\":[{\"name\":\"value\",\"type\":\"core::felt252\",\"kind\":\"data\"}]}]"
}`

// --- Helper ---

func feltFromUint64(v uint64) *felt.Felt {
	return new(felt.Felt).SetUint64(v)
}

func feltFromHex(hex string) *felt.Felt {
	f, _ := new(felt.Felt).SetString(hex)
	return f
}

func feltFromBigInt(bi *big.Int) *felt.Felt {
	return new(felt.Felt).SetBigInt(bi)
}

// --- Parser Tests ---

func TestParseRawABI(t *testing.T) {
	abi, err := Parse([]byte(erc20ABI))
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}

	// Should have u256 struct and bool enum in type registry.
	if _, ok := abi.Types["core::integer::u256"]; !ok {
		t.Error("expected u256 struct in type registry")
	}
	if _, ok := abi.Types["core::bool"]; !ok {
		t.Error("expected bool enum in type registry")
	}

	// Should have 2 struct-kind events (Transfer, Approval).
	// The enum-kind Event is not included as an emittable event.
	if len(abi.Events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(abi.Events))
	}
}

func TestParseContractClassJSON(t *testing.T) {
	abi, err := Parse([]byte(contractClassJSON))
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}

	if len(abi.Events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(abi.Events))
	}
	if abi.Events[0].Name != "SimpleEvent" {
		t.Errorf("expected event name SimpleEvent, got %s", abi.Events[0].Name)
	}
}

func TestParseEventMembers(t *testing.T) {
	abi, err := Parse([]byte(erc20ABI))
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}

	// Find Transfer event.
	var transfer *EventDef
	for _, ev := range abi.Events {
		if ev.Name == "Transfer" {
			transfer = ev
			break
		}
	}
	if transfer == nil {
		t.Fatal("Transfer event not found")
	}

	// Transfer should have 2 key members (from, to) and 1 data member (value).
	if len(transfer.KeyMembers) != 2 {
		t.Errorf("expected 2 key members, got %d", len(transfer.KeyMembers))
	}
	if len(transfer.DataMembers) != 1 {
		t.Errorf("expected 1 data member, got %d", len(transfer.DataMembers))
	}

	// Key members should be ContractAddress type.
	if transfer.KeyMembers[0].Type.Kind != CairoContractAddress {
		t.Errorf("expected from to be ContractAddress, got %d", transfer.KeyMembers[0].Type.Kind)
	}

	// Data member (value) should be u256 (decoded as primitive, not struct).
	if transfer.DataMembers[0].Type.Kind != CairoU256 {
		t.Errorf("expected value to be CairoU256, got %d", transfer.DataMembers[0].Type.Kind)
	}
}

func TestParseShortName(t *testing.T) {
	tests := []struct {
		fullName string
		expected string
	}{
		{"Transfer", "Transfer"},
		{"mymod::Transfer", "Transfer"},
		{"openzeppelin::token::erc20::erc20::ERC20Component::Transfer", "Transfer"},
	}
	for _, tt := range tests {
		got := shortName(tt.fullName)
		if got != tt.expected {
			t.Errorf("shortName(%q) = %q, want %q", tt.fullName, got, tt.expected)
		}
	}
}

func TestParseComplexABI(t *testing.T) {
	abi, err := Parse([]byte(complexABI))
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}

	if len(abi.Events) != 5 {
		t.Fatalf("expected 5 events, got %d", len(abi.Events))
	}

	// Verify GameState struct was registered.
	gs, ok := abi.Types["mycontract::GameState"]
	if !ok {
		t.Fatal("GameState struct not found")
	}
	if len(gs.Members) != 3 {
		t.Errorf("expected 3 GameState members, got %d", len(gs.Members))
	}
}

func TestParsePrimitiveTypes(t *testing.T) {
	tests := []struct {
		name     string
		expected CairoType
	}{
		{"core::felt252", CairoFelt252},
		{"core::integer::u8", CairoU8},
		{"core::integer::u16", CairoU16},
		{"core::integer::u32", CairoU32},
		{"core::integer::u64", CairoU64},
		{"core::integer::u128", CairoU128},
		{"core::integer::u256", CairoU256},
		{"core::integer::i8", CairoI8},
		{"core::integer::i16", CairoI16},
		{"core::integer::i32", CairoI32},
		{"core::integer::i64", CairoI64},
		{"core::integer::i128", CairoI128},
		{"core::bool", CairoBool},
		{"core::starknet::contract_address::ContractAddress", CairoContractAddress},
		{"core::starknet::class_hash::ClassHash", CairoClassHash},
		{"core::byte_array::ByteArray", CairoByteArray},
		{"()", CairoUnit},
	}
	for _, tt := range tests {
		td := resolvePrimitive(tt.name)
		if td.Kind != tt.expected {
			t.Errorf("resolvePrimitive(%q).Kind = %d, want %d", tt.name, td.Kind, tt.expected)
		}
	}
}

func TestParseGenericTypes(t *testing.T) {
	arrayTD := resolvePrimitive("core::array::Array::<core::integer::u64>")
	if arrayTD.Kind != CairoArray {
		t.Errorf("expected Array, got %d", arrayTD.Kind)
	}
	if arrayTD.Inner == nil || arrayTD.Inner.Kind != CairoU64 {
		t.Error("expected inner type u64")
	}

	spanTD := resolvePrimitive("core::array::Span::<core::felt252>")
	if spanTD.Kind != CairoSpan {
		t.Errorf("expected Span, got %d", spanTD.Kind)
	}
	if spanTD.Inner == nil || spanTD.Inner.Kind != CairoFelt252 {
		t.Error("expected inner type felt252")
	}
}

func TestParseSnapshotTypes(t *testing.T) {
	// Snapshot types are stripped of @ prefix.
	abiJSON := `[
		{
			"type": "event",
			"name": "test::SnapEvent",
			"kind": "struct",
			"members": [
				{"name": "data", "type": "@core::array::Array::<core::integer::u64>", "kind": "data"}
			]
		}
	]`
	parsed, err := Parse([]byte(abiJSON))
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}

	ev := parsed.Events[0]
	if ev.DataMembers[0].Type.Kind != CairoArray {
		t.Errorf("expected snapshot Array to resolve to Array, got %d", ev.DataMembers[0].Type.Kind)
	}
}

func TestParseInvalidJSON(t *testing.T) {
	_, err := Parse([]byte("not json"))
	if err == nil {
		t.Error("expected error for invalid JSON")
	}
}

// --- Selector Tests ---

func TestComputeSelector(t *testing.T) {
	// Known selector for "Transfer" event.
	expected := utils.GetSelectorFromNameFelt("Transfer")
	got := ComputeSelector("Transfer")

	if !got.Equal(expected) {
		t.Errorf("selector mismatch: got %s, want %s", got.String(), expected.String())
	}
}

func TestEventRegistry(t *testing.T) {
	parsed, err := Parse([]byte(erc20ABI))
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}

	reg := NewEventRegistry(parsed)

	// Match by selector.
	transferSelector := ComputeSelector("Transfer")
	ev := reg.MatchSelector(transferSelector)
	if ev == nil {
		t.Fatal("expected to match Transfer event by selector")
	}
	if ev.Name != "Transfer" {
		t.Errorf("expected Transfer, got %s", ev.Name)
	}

	// Match by name.
	ev = reg.MatchName("Approval")
	if ev == nil {
		t.Fatal("expected to match Approval event by name")
	}

	// No match for unknown.
	ev = reg.MatchSelector(ComputeSelector("Unknown"))
	if ev != nil {
		t.Error("expected nil for unknown selector")
	}
	ev = reg.MatchName("Unknown")
	if ev != nil {
		t.Error("expected nil for unknown name")
	}

	// Nil selector.
	ev = reg.MatchSelector(nil)
	if ev != nil {
		t.Error("expected nil for nil selector")
	}
}

func TestEventRegistryEvents(t *testing.T) {
	parsed, err := Parse([]byte(erc20ABI))
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}

	reg := NewEventRegistry(parsed)
	events := reg.Events()
	if len(events) != 2 {
		t.Errorf("expected 2 events, got %d", len(events))
	}
}

// --- Decoder Tests ---

func TestDecodeTransferEvent(t *testing.T) {
	parsed, err := Parse([]byte(erc20ABI))
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}

	reg := NewEventRegistry(parsed)
	ev := reg.MatchName("Transfer")

	// Simulate Transfer event:
	// keys = [from_address, to_address]
	// data = [value_low, value_high]
	from := feltFromHex("0xabc")
	to := feltFromHex("0xdef")
	valueLow := feltFromUint64(1000)
	valueHigh := feltFromUint64(0)

	result, err := DecodeEvent(ev, []*felt.Felt{from, to}, []*felt.Felt{valueLow, valueHigh})
	if err != nil {
		t.Fatalf("DecodeEvent failed: %v", err)
	}

	// Check from address.
	if result["from"] != from.String() {
		t.Errorf("from = %v, want %s", result["from"], from.String())
	}
	// Check to address.
	if result["to"] != to.String() {
		t.Errorf("to = %v, want %s", result["to"], to.String())
	}
	// Check value (u256 = "1000").
	if result["value"] != "1000" {
		t.Errorf("value = %v, want 1000", result["value"])
	}
}

func TestDecodeUintTypes(t *testing.T) {
	parsed, err := Parse([]byte(complexABI))
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}

	reg := NewEventRegistry(parsed)
	ev := reg.MatchName("ScoreUpdated")

	player := feltFromHex("0x123")
	oldScore := feltFromUint64(50)
	newScore := feltFromUint64(100)
	isHighscore := feltFromUint64(1) // bool True

	result, err := DecodeEvent(ev,
		[]*felt.Felt{player},
		[]*felt.Felt{oldScore, newScore, isHighscore},
	)
	if err != nil {
		t.Fatalf("DecodeEvent failed: %v", err)
	}

	if result["player"] != player.String() {
		t.Errorf("player = %v, want %s", result["player"], player.String())
	}
	if result["old_score"] != uint64(50) {
		t.Errorf("old_score = %v, want 50", result["old_score"])
	}
	if result["new_score"] != uint64(100) {
		t.Errorf("new_score = %v, want 100", result["new_score"])
	}
	if result["is_highscore"] != true {
		t.Errorf("is_highscore = %v, want true", result["is_highscore"])
	}
}

func TestDecodeSignedTypes(t *testing.T) {
	parsed, err := Parse([]byte(complexABI))
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}

	reg := NewEventRegistry(parsed)
	ev := reg.MatchName("SignedEvent")

	// i8: -1 is represented as 255 (2^8 - 1)
	i8Neg := feltFromUint64(255)
	// i32: -100 is 2^32 - 100 = 4294967196
	i32Neg := feltFromUint64(4294967196)
	// i128: -1 is 2^128 - 1
	i128Neg := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 128), big.NewInt(1))

	result, err := DecodeEvent(ev,
		[]*felt.Felt{},
		[]*felt.Felt{i8Neg, i32Neg, feltFromBigInt(i128Neg)},
	)
	if err != nil {
		t.Fatalf("DecodeEvent failed: %v", err)
	}

	if result["value_i8"] != int64(-1) {
		t.Errorf("value_i8 = %v, want -1", result["value_i8"])
	}
	if result["value_i32"] != int64(-100) {
		t.Errorf("value_i32 = %v, want -100", result["value_i32"])
	}
	if result["value_i128"] != "-1" {
		t.Errorf("value_i128 = %v, want -1", result["value_i128"])
	}
}

func TestDecodePositiveSignedTypes(t *testing.T) {
	parsed, err := Parse([]byte(complexABI))
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}

	reg := NewEventRegistry(parsed)
	ev := reg.MatchName("SignedEvent")

	result, err := DecodeEvent(ev,
		[]*felt.Felt{},
		[]*felt.Felt{feltFromUint64(42), feltFromUint64(1000), feltFromUint64(999)},
	)
	if err != nil {
		t.Fatalf("DecodeEvent failed: %v", err)
	}

	if result["value_i8"] != int64(42) {
		t.Errorf("value_i8 = %v, want 42", result["value_i8"])
	}
	if result["value_i32"] != int64(1000) {
		t.Errorf("value_i32 = %v, want 1000", result["value_i32"])
	}
	if result["value_i128"] != "999" {
		t.Errorf("value_i128 = %v, want 999", result["value_i128"])
	}
}

func TestDecodeByteArray(t *testing.T) {
	parsed, err := Parse([]byte(complexABI))
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}

	reg := NewEventRegistry(parsed)
	ev := reg.MatchName("MessagePosted")

	sender := feltFromHex("0x123")

	// ByteArray encoding for "hello":
	// num_chunks = 0 (message fits in pending word)
	// pending_word = felt with "hello" bytes
	// pending_word_len = 5
	numChunks := feltFromUint64(0)
	pendingWord := new(felt.Felt).SetBytes([]byte{
		0, 0, 0, 0, 0, 0, 0, 0,
		0, 0, 0, 0, 0, 0, 0, 0,
		0, 0, 0, 0, 0, 0, 0, 0,
		0, 0, 0, 'h', 'e', 'l', 'l', 'o',
	})
	pendingLen := feltFromUint64(5)

	result, err := DecodeEvent(ev,
		[]*felt.Felt{sender},
		[]*felt.Felt{numChunks, pendingWord, pendingLen},
	)
	if err != nil {
		t.Fatalf("DecodeEvent failed: %v", err)
	}

	if result["message"] != "hello" {
		t.Errorf("message = %q, want %q", result["message"], "hello")
	}
}

func TestDecodeByteArrayWithChunks(t *testing.T) {
	parsed, err := Parse([]byte(complexABI))
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}

	reg := NewEventRegistry(parsed)
	ev := reg.MatchName("MessagePosted")

	sender := feltFromHex("0x123")

	// ByteArray with 1 full chunk (31 bytes) + pending word (4 bytes) = 35 bytes.
	numChunks := feltFromUint64(1)

	// Full chunk: 31 bytes of 'A'.
	chunkBytes := make([]byte, 32)
	for i := 1; i < 32; i++ {
		chunkBytes[i] = 'A' // First byte is 0 for 31-byte chunks.
	}
	chunk := new(felt.Felt).SetBytes(chunkBytes)

	// Pending: "BCDE" (4 bytes).
	pendingWord := new(felt.Felt).SetBytes([]byte{
		0, 0, 0, 0, 0, 0, 0, 0,
		0, 0, 0, 0, 0, 0, 0, 0,
		0, 0, 0, 0, 0, 0, 0, 0,
		0, 0, 0, 0, 'B', 'C', 'D', 'E',
	})
	pendingLen := feltFromUint64(4)

	result, err := DecodeEvent(ev,
		[]*felt.Felt{sender},
		[]*felt.Felt{numChunks, chunk, pendingWord, pendingLen},
	)
	if err != nil {
		t.Fatalf("DecodeEvent failed: %v", err)
	}

	msg := result["message"].(string)
	// 31 'A's + "BCDE" = 35 chars.
	expectedLen := 35
	if len(msg) != expectedLen {
		t.Errorf("message length = %d, want %d", len(msg), expectedLen)
	}
	// Verify chunk content.
	for i := 0; i < 31; i++ {
		if msg[i] != 'A' {
			t.Errorf("message[%d] = %c, want A", i, msg[i])
			break
		}
	}
	if msg[31:] != "BCDE" {
		t.Errorf("message pending = %q, want BCDE", msg[31:])
	}
}

func TestDecodeArray(t *testing.T) {
	parsed, err := Parse([]byte(complexABI))
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}

	reg := NewEventRegistry(parsed)
	ev := reg.MatchName("BatchTransfer")

	sender := feltFromHex("0x123")

	// Array<u64> with 3 elements: [100, 200, 300]
	arrLen := feltFromUint64(3)
	elem1 := feltFromUint64(100)
	elem2 := feltFromUint64(200)
	elem3 := feltFromUint64(300)

	result, err := DecodeEvent(ev,
		[]*felt.Felt{sender},
		[]*felt.Felt{arrLen, elem1, elem2, elem3},
	)
	if err != nil {
		t.Fatalf("DecodeEvent failed: %v", err)
	}

	amounts, ok := result["amounts"].([]any)
	if !ok {
		t.Fatalf("amounts is not []any: %T", result["amounts"])
	}
	if len(amounts) != 3 {
		t.Fatalf("expected 3 amounts, got %d", len(amounts))
	}
	expected := []uint64{100, 200, 300}
	for i, exp := range expected {
		if amounts[i] != exp {
			t.Errorf("amounts[%d] = %v, want %d", i, amounts[i], exp)
		}
	}
}

func TestDecodeEmptyArray(t *testing.T) {
	parsed, err := Parse([]byte(complexABI))
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}

	reg := NewEventRegistry(parsed)
	ev := reg.MatchName("BatchTransfer")

	sender := feltFromHex("0x123")
	arrLen := feltFromUint64(0)

	result, err := DecodeEvent(ev,
		[]*felt.Felt{sender},
		[]*felt.Felt{arrLen},
	)
	if err != nil {
		t.Fatalf("DecodeEvent failed: %v", err)
	}

	amounts, ok := result["amounts"].([]any)
	if !ok {
		t.Fatalf("amounts is not []any: %T", result["amounts"])
	}
	if len(amounts) != 0 {
		t.Errorf("expected empty array, got %d elements", len(amounts))
	}
}

func TestDecodeNestedStruct(t *testing.T) {
	parsed, err := Parse([]byte(complexABI))
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}

	reg := NewEventRegistry(parsed)
	ev := reg.MatchName("StructEvent")

	// GameState struct: player (ContractAddress), score (u64), active (bool)
	player := feltFromHex("0xabc")
	score := feltFromUint64(42)
	active := feltFromUint64(1) // True

	result, err := DecodeEvent(ev,
		[]*felt.Felt{},
		[]*felt.Felt{player, score, active},
	)
	if err != nil {
		t.Fatalf("DecodeEvent failed: %v", err)
	}

	state, ok := result["state"].(map[string]any)
	if !ok {
		t.Fatalf("state is not map[string]any: %T", result["state"])
	}
	if state["player"] != player.String() {
		t.Errorf("state.player = %v, want %s", state["player"], player.String())
	}
	if state["score"] != uint64(42) {
		t.Errorf("state.score = %v, want 42", state["score"])
	}
	if state["active"] != true {
		t.Errorf("state.active = %v, want true", state["active"])
	}
}

func TestDecodeBoolFalse(t *testing.T) {
	parsed, err := Parse([]byte(complexABI))
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}

	reg := NewEventRegistry(parsed)
	ev := reg.MatchName("ScoreUpdated")

	result, err := DecodeEvent(ev,
		[]*felt.Felt{feltFromHex("0x1")},
		[]*felt.Felt{feltFromUint64(0), feltFromUint64(10), feltFromUint64(0)},
	)
	if err != nil {
		t.Fatalf("DecodeEvent failed: %v", err)
	}

	if result["is_highscore"] != false {
		t.Errorf("is_highscore = %v, want false", result["is_highscore"])
	}
}

func TestDecodeU256Large(t *testing.T) {
	parsed, err := Parse([]byte(erc20ABI))
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}

	reg := NewEventRegistry(parsed)
	ev := reg.MatchName("Transfer")

	from := feltFromHex("0x1")
	to := feltFromHex("0x2")

	// u256 with large value: high=1, low=0 => 2^128
	valueLow := feltFromUint64(0)
	valueHigh := feltFromUint64(1)

	result, err := DecodeEvent(ev,
		[]*felt.Felt{from, to},
		[]*felt.Felt{valueLow, valueHigh},
	)
	if err != nil {
		t.Fatalf("DecodeEvent failed: %v", err)
	}

	expected := new(big.Int).Lsh(big.NewInt(1), 128)
	if result["value"] != expected.String() {
		t.Errorf("value = %v, want %s", result["value"], expected.String())
	}
}

func TestDecodeOutOfBounds(t *testing.T) {
	parsed, err := Parse([]byte(erc20ABI))
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}

	reg := NewEventRegistry(parsed)
	ev := reg.MatchName("Transfer")

	// Not enough data felts (need 2 for u256, only provide 1).
	_, err = DecodeEvent(ev,
		[]*felt.Felt{feltFromHex("0x1"), feltFromHex("0x2")},
		[]*felt.Felt{feltFromUint64(100)},
	)
	if err == nil {
		t.Error("expected error for insufficient data felts")
	}
}

func TestDecodeEmptyKeys(t *testing.T) {
	// Event with no key members, only data members.
	parsed, err := Parse([]byte(complexABI))
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}

	reg := NewEventRegistry(parsed)
	ev := reg.MatchName("SignedEvent")

	result, err := DecodeEvent(ev,
		[]*felt.Felt{},
		[]*felt.Felt{feltFromUint64(42), feltFromUint64(100), feltFromUint64(999)},
	)
	if err != nil {
		t.Fatalf("DecodeEvent failed: %v", err)
	}

	if result["value_i8"] != int64(42) {
		t.Errorf("value_i8 = %v, want 42", result["value_i8"])
	}
}

// --- View Function Tests ---

// ABI with view functions for testing.
const viewFunctionABI = `[
	{
		"type": "struct",
		"name": "core::integer::u256",
		"members": [
			{"name": "low", "type": "core::integer::u128"},
			{"name": "high", "type": "core::integer::u128"}
		]
	},
	{
		"type": "function",
		"name": "mycontract::MyContract::get_price",
		"inputs": [
			{"name": "asset_id", "type": "core::felt252"}
		],
		"outputs": [
			{"name": "price", "type": "core::integer::u256"},
			{"name": "timestamp", "type": "core::integer::u64"}
		],
		"state_mutability": "view"
	},
	{
		"type": "function",
		"name": "mycontract::MyContract::total_supply",
		"inputs": [],
		"outputs": [
			{"name": "supply", "type": "core::integer::u256"}
		],
		"state_mutability": "view"
	},
	{
		"type": "function",
		"name": "mycontract::MyContract::transfer",
		"inputs": [
			{"name": "to", "type": "core::starknet::contract_address::ContractAddress"},
			{"name": "amount", "type": "core::integer::u256"}
		],
		"outputs": [],
		"state_mutability": "external"
	},
	{
		"type": "interface",
		"name": "mycontract::IMyContract",
		"items": [
			{
				"type": "function",
				"name": "mycontract::IMyContract::get_balance",
				"inputs": [
					{"name": "account", "type": "core::starknet::contract_address::ContractAddress"}
				],
				"outputs": [
					{"name": "balance", "type": "core::integer::u256"}
				],
				"state_mutability": "view"
			}
		]
	}
]`

func TestParseViewFunctions(t *testing.T) {
	parsed, err := Parse([]byte(viewFunctionABI))
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}

	// Should have 3 view functions: get_price, total_supply, get_balance (from interface).
	// The "transfer" function is external and should be excluded.
	if len(parsed.Functions) != 3 {
		t.Fatalf("expected 3 view functions, got %d", len(parsed.Functions))
	}

	// Check get_price.
	fn, ok := parsed.Functions["get_price"]
	if !ok {
		t.Fatal("get_price function not found")
	}
	if fn.StateMutability != "view" {
		t.Errorf("get_price state_mutability = %q, want view", fn.StateMutability)
	}
	if len(fn.Inputs) != 1 {
		t.Errorf("get_price inputs count = %d, want 1", len(fn.Inputs))
	}
	if len(fn.Outputs) != 2 {
		t.Errorf("get_price outputs count = %d, want 2", len(fn.Outputs))
	}
	if fn.Inputs[0].Name != "asset_id" {
		t.Errorf("get_price input name = %q, want asset_id", fn.Inputs[0].Name)
	}
	if fn.Outputs[0].Name != "price" {
		t.Errorf("get_price output[0] name = %q, want price", fn.Outputs[0].Name)
	}
	if fn.Outputs[0].Type.Kind != CairoU256 {
		t.Errorf("get_price output[0] type = %d, want CairoU256", fn.Outputs[0].Type.Kind)
	}
	if fn.Selector == nil {
		t.Error("get_price selector is nil")
	}

	// Check total_supply (no inputs).
	fn, ok = parsed.Functions["total_supply"]
	if !ok {
		t.Fatal("total_supply function not found")
	}
	if len(fn.Inputs) != 0 {
		t.Errorf("total_supply inputs count = %d, want 0", len(fn.Inputs))
	}
	if len(fn.Outputs) != 1 {
		t.Errorf("total_supply outputs count = %d, want 1", len(fn.Outputs))
	}

	// Check get_balance (from interface).
	fn, ok = parsed.Functions["get_balance"]
	if !ok {
		t.Fatal("get_balance function not found (from interface)")
	}
	if len(fn.Inputs) != 1 || fn.Inputs[0].Name != "account" {
		t.Errorf("get_balance input = %+v, want [account]", fn.Inputs)
	}

	// External function should NOT be in Functions map.
	if _, ok := parsed.Functions["transfer"]; ok {
		t.Error("transfer (external) should not be in Functions map")
	}
}

func TestDecodeFunctionOutputs_SingleFelt(t *testing.T) {
	outputs := []FieldDef{
		{Name: "value", Type: &TypeDef{Kind: CairoFelt252, Name: "core::felt252"}},
	}
	felts := []*felt.Felt{feltFromHex("0xdeadbeef")}

	result, err := DecodeFunctionOutputs("fn", outputs, felts)
	if err != nil {
		t.Fatalf("DecodeFunctionOutputs failed: %v", err)
	}
	if result["value"] != feltFromHex("0xdeadbeef").String() {
		t.Errorf("value = %v, want 0xdeadbeef hex string", result["value"])
	}
}

func TestDecodeFunctionOutputs_U256(t *testing.T) {
	outputs := []FieldDef{
		{Name: "supply", Type: &TypeDef{Kind: CairoU256, Name: "core::integer::u256"}},
	}
	felts := []*felt.Felt{feltFromUint64(1000), feltFromUint64(0)}

	result, err := DecodeFunctionOutputs("fn", outputs, felts)
	if err != nil {
		t.Fatalf("DecodeFunctionOutputs failed: %v", err)
	}
	if result["supply"] != "1000" {
		t.Errorf("supply = %v, want 1000", result["supply"])
	}
}

func TestDecodeFunctionOutputs_Multiple(t *testing.T) {
	outputs := []FieldDef{
		{Name: "price", Type: &TypeDef{Kind: CairoU256, Name: "core::integer::u256"}},
		{Name: "timestamp", Type: &TypeDef{Kind: CairoU64, Name: "core::integer::u64"}},
	}
	felts := []*felt.Felt{feltFromUint64(42000), feltFromUint64(0), feltFromUint64(1710072000)}

	result, err := DecodeFunctionOutputs("fn", outputs, felts)
	if err != nil {
		t.Fatalf("DecodeFunctionOutputs failed: %v", err)
	}
	if result["price"] != "42000" {
		t.Errorf("price = %v, want 42000", result["price"])
	}
	if result["timestamp"] != uint64(1710072000) {
		t.Errorf("timestamp = %v, want 1710072000", result["timestamp"])
	}
}

func TestDecodeFunctionOutputs_Empty(t *testing.T) {
	result, err := DecodeFunctionOutputs(nil, nil)
	if err != nil {
		t.Fatalf("DecodeFunctionOutputs failed: %v", err)
	}
	if len(result) != 0 {
		t.Errorf("expected empty result, got %d entries", len(result))
	}
}

func TestDecodeFunctionOutputs_InsufficientFelts(t *testing.T) {
	outputs := []FieldDef{
		{Name: "value", Type: &TypeDef{Kind: CairoU256, Name: "core::integer::u256"}},
	}
	felts := []*felt.Felt{feltFromUint64(100)} // Need 2 felts for u256

	_, err := DecodeFunctionOutputs("fn", outputs, felts)
	if err == nil {
		t.Fatal("expected error for insufficient felts")
	}
}

func TestEncodeFunctionCalldata(t *testing.T) {
	args := []string{"0xabc", "0x123"}
	result, err := EncodeFunctionCalldata(args)
	if err != nil {
		t.Fatalf("EncodeFunctionCalldata failed: %v", err)
	}
	if len(result) != 2 {
		t.Fatalf("expected 2 felts, got %d", len(result))
	}
	if result[0].String() != feltFromHex("0xabc").String() {
		t.Errorf("result[0] = %s, want 0xabc", result[0].String())
	}
	if result[1].String() != feltFromHex("0x123").String() {
		t.Errorf("result[1] = %s, want 0x123", result[1].String())
	}
}

func TestEncodeFunctionCalldata_Empty(t *testing.T) {
	result, err := EncodeFunctionCalldata(nil)
	if err != nil {
		t.Fatalf("EncodeFunctionCalldata failed: %v", err)
	}
	if len(result) != 0 {
		t.Errorf("expected empty result, got %d", len(result))
	}
}

func TestEncodeFunctionCalldata_InvalidHex(t *testing.T) {
	_, err := EncodeFunctionCalldata([]string{"not-hex"})
	if err == nil {
		t.Fatal("expected error for non-hex calldata")
	}
}

func TestEncodeFunctionCalldata_LargeFelt(t *testing.T) {
	// A valid felt value (must be < P, the Stark prime ~2^251).
	args := []string{"0x0454480000000000000000000000000000000000000000000000000000000000"}
	result, err := EncodeFunctionCalldata(args)
	if err != nil {
		t.Fatalf("EncodeFunctionCalldata failed: %v", err)
	}
	if len(result) != 1 {
		t.Fatalf("expected 1 felt, got %d", len(result))
	}
}

func TestFunctionDefSelector(t *testing.T) {
	parsed, err := Parse([]byte(viewFunctionABI))
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}

	fn := parsed.Functions["get_price"]
	expected := ComputeSelector("get_price")
	if !fn.Selector.Equal(expected) {
		t.Errorf("get_price selector = %s, want %s", fn.Selector.String(), expected.String())
	}
}

// --- FeltSize Tests ---

func TestFeltSize(t *testing.T) {
	tests := []struct {
		kind     CairoType
		expected int
	}{
		{CairoUnit, 0},
		{CairoFelt252, 1},
		{CairoU8, 1},
		{CairoU64, 1},
		{CairoU128, 1},
		{CairoBool, 1},
		{CairoContractAddress, 1},
		{CairoU256, 2},
		{CairoArray, -1},
		{CairoByteArray, -1},
	}
	for _, tt := range tests {
		td := &TypeDef{Kind: tt.kind}
		if got := td.FeltSize(); got != tt.expected {
			t.Errorf("FeltSize(%d) = %d, want %d", tt.kind, got, tt.expected)
		}
	}
}

func TestFeltSizeStruct(t *testing.T) {
	// u256 struct: low (u128) + high (u128) = 2 felts.
	u256 := &TypeDef{
		Kind: CairoStruct,
		Members: []FieldDef{
			{Name: "low", Type: &TypeDef{Kind: CairoU128}},
			{Name: "high", Type: &TypeDef{Kind: CairoU128}},
		},
	}
	if u256.FeltSize() != 2 {
		t.Errorf("u256 FeltSize = %d, want 2", u256.FeltSize())
	}
}

// --- Tuple decoding tests ---

func TestDecodeFunctionOutputs_ArrayOfTupleU256(t *testing.T) {
	// Reproduces the bug: Array<(u256, u256)> view return type.
	// Type: core::array::Array::<(core::integer::u256, core::integer::u256)>
	// Raw felts from RPC: [array_len=1, price_low, price_high, volume_low, volume_high]

	// Build the type through the parser to test full resolution.
	abiJSON := `[
		{
			"type": "interface",
			"name": "IOrderBook",
			"items": [
				{
					"type": "function",
					"name": "get_depth",
					"inputs": [{"name": "max_levels", "type": "core::integer::u32"}],
					"outputs": [{"name": "depth", "type": "core::array::Array::<(core::integer::u256, core::integer::u256)>"}],
					"state_mutability": "view"
				}
			]
		}
	]`
	parsed, err := Parse([]byte(abiJSON))
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}
	fn := parsed.Functions["get_depth"]
	if fn == nil {
		t.Fatal("get_depth function not found")
	}

	// Simulate RPC return: 1 tuple element with price=7340000000, volume=99787251
	felts := []*felt.Felt{
		feltFromUint64(1),          // array length
		feltFromHex("0x1b57f8300"), // price low
		feltFromUint64(0),          // price high
		feltFromHex("0x5f2a1f3"),   // volume low
		feltFromUint64(0),          // volume high
	}

	result, err := DecodeFunctionOutputs(fn.Outputs, felts)
	if err != nil {
		t.Fatalf("DecodeFunctionOutputs failed: %v", err)
	}

	depth, ok := result["depth"].([]any)
	if !ok {
		t.Fatalf("depth is not []any, got %T: %v", result["depth"], result["depth"])
	}
	if len(depth) != 1 {
		t.Fatalf("depth length = %d, want 1", len(depth))
	}

	tuple, ok := depth[0].(map[string]any)
	if !ok {
		t.Fatalf("tuple is not map[string]any, got %T: %v", depth[0], depth[0])
	}

	// u256: (0x1b57f8300 low, 0 high) = 7340000000
	if tuple["0"] != "7340000000" {
		t.Errorf("tuple[0] = %v, want 7340000000", tuple["0"])
	}
	// u256: (0x5f2a1f3 low, 0 high) = 99787251
	if tuple["1"] != "99787251" {
		t.Errorf("tuple[1] = %v, want 99787251", tuple["1"])
	}
}

func TestDecodeFunctionOutputs_ArrayOfTupleMultipleElements(t *testing.T) {
	// Test with multiple tuple elements in the array.
	tupleType := &TypeDef{
		Kind: CairoTuple,
		Name: "(core::integer::u256, core::integer::u256)",
		Members: []FieldDef{
			{Name: "0", Type: &TypeDef{Kind: CairoU256, Name: "core::integer::u256"}},
			{Name: "1", Type: &TypeDef{Kind: CairoU256, Name: "core::integer::u256"}},
		},
	}
	outputs := []FieldDef{
		{Name: "positions", Type: &TypeDef{Kind: CairoArray, Name: "array", Inner: tupleType}},
	}

	felts := []*felt.Felt{
		feltFromUint64(2),   // array length = 2
		feltFromUint64(100), // pos[0].first low
		feltFromUint64(0),   // pos[0].first high
		feltFromUint64(200), // pos[0].second low
		feltFromUint64(0),   // pos[0].second high
		feltFromUint64(300), // pos[1].first low
		feltFromUint64(0),   // pos[1].first high
		feltFromUint64(400), // pos[1].second low
		feltFromUint64(0),   // pos[1].second high
	}

	result, err := DecodeFunctionOutputs("fn", outputs, felts)
	if err != nil {
		t.Fatalf("DecodeFunctionOutputs failed: %v", err)
	}

	positions := result["positions"].([]any)
	if len(positions) != 2 {
		t.Fatalf("positions length = %d, want 2", len(positions))
	}

	pos0 := positions[0].(map[string]any)
	if pos0["0"] != "100" {
		t.Errorf("pos[0][0] = %v, want 100", pos0["0"])
	}
	if pos0["1"] != "200" {
		t.Errorf("pos[0][1] = %v, want 200", pos0["1"])
	}

	pos1 := positions[1].(map[string]any)
	if pos1["0"] != "300" {
		t.Errorf("pos[1][0] = %v, want 300", pos1["0"])
	}
	if pos1["1"] != "400" {
		t.Errorf("pos[1][1] = %v, want 400", pos1["1"])
	}
}

func TestResolveTupleType(t *testing.T) {
	// Test that tuple types are correctly resolved via the parser.
	abiJSON := `[
		{
			"type": "interface",
			"name": "ITest",
			"items": [
				{
					"type": "function",
					"name": "get_pair",
					"inputs": [],
					"outputs": [{"name": "pair", "type": "(core::felt252, core::integer::u64)"}],
					"state_mutability": "view"
				}
			]
		}
	]`
	parsed, err := Parse([]byte(abiJSON))
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}
	fn := parsed.Functions["get_pair"]
	if fn == nil {
		t.Fatal("get_pair not found")
	}

	td := fn.Outputs[0].Type
	if td.Kind != CairoTuple {
		t.Fatalf("expected CairoTuple, got %d", td.Kind)
	}
	if len(td.Members) != 2 {
		t.Fatalf("expected 2 members, got %d", len(td.Members))
	}
	if td.Members[0].Type.Kind != CairoFelt252 {
		t.Errorf("member 0 kind = %d, want CairoFelt252", td.Members[0].Type.Kind)
	}
	if td.Members[1].Type.Kind != CairoU64 {
		t.Errorf("member 1 kind = %d, want CairoU64", td.Members[1].Type.Kind)
	}
}

func TestFeltSizeTuple(t *testing.T) {
	td := &TypeDef{
		Kind: CairoTuple,
		Members: []FieldDef{
			{Name: "0", Type: &TypeDef{Kind: CairoU256}},
			{Name: "1", Type: &TypeDef{Kind: CairoU256}},
		},
	}
	if td.FeltSize() != 4 {
		t.Errorf("(u256, u256) FeltSize = %d, want 4", td.FeltSize())
	}
}

// --- End-to-end test: parse, register, match, decode ---

func TestEndToEnd(t *testing.T) {
	// Parse ABI.
	parsed, err := Parse([]byte(erc20ABI))
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}

	// Build registry.
	reg := NewEventRegistry(parsed)

	// Simulate receiving a raw event from the chain.
	selector := utils.GetSelectorFromNameFelt("Transfer")
	from := feltFromHex("0x049d36570d4e46f48e99674bd3fcc84644ddd6b96f7c741b1562b82f9e004dc7")
	to := feltFromHex("0x0000000000000000000000000000000000000000000000000000000000001234")
	valueLow := feltFromUint64(1000000000000000000) // 1e18
	valueHigh := feltFromUint64(0)

	// Match by selector.
	ev := reg.MatchSelector(selector)
	if ev == nil {
		t.Fatal("failed to match Transfer selector")
	}
	if ev.Name != "Transfer" {
		t.Fatalf("expected Transfer, got %s", ev.Name)
	}

	// Decode (keys[1:] because keys[0] is selector).
	result, err := DecodeEvent(ev, []*felt.Felt{from, to}, []*felt.Felt{valueLow, valueHigh})
	if err != nil {
		t.Fatalf("DecodeEvent failed: %v", err)
	}

	// Verify decoded fields.
	if result["from"] != from.String() {
		t.Errorf("from mismatch")
	}
	if result["to"] != to.String() {
		t.Errorf("to mismatch")
	}
	if result["value"] != "1000000000000000000" {
		t.Errorf("value = %v, want 1000000000000000000", result["value"])
	}
}
