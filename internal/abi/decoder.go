package abi

import (
	"fmt"
	"math/big"
	"strings"

	"github.com/NethermindEth/juno/core/felt"
)

// DecodeEvent decodes raw event keys and data felts into a typed map
// using the matched event definition.
// keys should NOT include keys[0] (the selector) -- pass keys[1:].
func DecodeEvent(ev *EventDef, keys, data []*felt.Felt) (map[string]any, error) {
	result := make(map[string]any, len(ev.KeyMembers)+len(ev.DataMembers))

	// Decode key members from keys[].
	keyOffset := 0
	for _, member := range ev.KeyMembers {
		val, consumed, err := decodeType(member.Type, keys, keyOffset)
		if err != nil {
			return nil, fmt.Errorf("decoding key member %q: %w", member.Name, err)
		}
		result[member.Name] = val
		keyOffset += consumed
	}

	// Decode data members from data[].
	dataOffset := 0
	for _, member := range ev.DataMembers {
		val, consumed, err := decodeType(member.Type, data, dataOffset)
		if err != nil {
			return nil, fmt.Errorf("decoding data member %q: %w", member.Name, err)
		}
		result[member.Name] = val
		dataOffset += consumed
	}

	return result, nil
}

// DecodeFunctionOutputs decodes the flat []*felt.Felt return value of a
// starknet_call into a typed map using the function's output member definitions.
// funcName is used as a fallback column name when the output members are
// unnamed in the ABI (common in Cairo, where `fn foo() -> u256` emits
// outputs: [{type: u256}] with no name field). When unnamed and there are
// multiple outputs we fall back to output_0, output_1, ... — matching the
// naming scheme used by schema.BuildViewSchema so decoded values land in the
// expected table columns.
func DecodeFunctionOutputs(funcName string, outputs []FieldDef, felts []*felt.Felt) (map[string]any, error) {
	result := make(map[string]any, len(outputs))
	offset := 0
	for i, member := range outputs {
		val, consumed, err := decodeType(member.Type, felts, offset)
		if err != nil {
			return nil, fmt.Errorf("decoding output member %q: %w", member.Name, err)
		}
		name := member.Name
		if name == "" {
			if len(outputs) == 1 {
				name = funcName
			} else {
				name = fmt.Sprintf("output_%d", i)
			}
		}
		result[name] = val
		offset += consumed
	}
	return result, nil
}

// decodeType decodes a value of the given type from felts starting at offset.
// Returns the decoded value and the number of felts consumed.
func decodeType(td *TypeDef, felts []*felt.Felt, offset int) (val any, consumed int, err error) {
	switch td.Kind {
	case CairoFelt252, CairoContractAddress, CairoClassHash:
		return decodeFelt(felts, offset)
	case CairoU8:
		return decodeUint(felts, offset, 8)
	case CairoU16:
		return decodeUint(felts, offset, 16)
	case CairoU32:
		return decodeUint(felts, offset, 32)
	case CairoU64:
		return decodeUint(felts, offset, 64)
	case CairoU128:
		return decodeU128(felts, offset)
	case CairoU256:
		return decodeU256(felts, offset)
	case CairoI8:
		return decodeSigned(felts, offset, 8)
	case CairoI16:
		return decodeSigned(felts, offset, 16)
	case CairoI32:
		return decodeSigned(felts, offset, 32)
	case CairoI64:
		return decodeSigned(felts, offset, 64)
	case CairoI128:
		return decodeI128(felts, offset)
	case CairoBool:
		return decodeBool(felts, offset)
	case CairoByteArray:
		return decodeByteArray(felts, offset)
	case CairoArray, CairoSpan:
		return decodeArray(td, felts, offset)
	case CairoTuple:
		return decodeTuple(td, felts, offset)
	case CairoStruct:
		return decodeStruct(td, felts, offset)
	case CairoEnum:
		return decodeEnum(td, felts, offset)
	case CairoUnit:
		return nil, 0, nil
	default:
		return decodeFelt(felts, offset)
	}
}

// decodeFelt decodes a single felt as a hex string.
func decodeFelt(felts []*felt.Felt, offset int) (val string, consumed int, err error) {
	if offset >= len(felts) {
		return "", 0, fmt.Errorf("offset %d out of bounds (len=%d)", offset, len(felts))
	}
	return felts[offset].String(), 1, nil
}

// decodeUint decodes a felt as an unsigned integer (8-64 bit range -> uint64).
func decodeUint(felts []*felt.Felt, offset, bits int) (val uint64, consumed int, err error) {
	if offset >= len(felts) {
		return 0, 0, fmt.Errorf("offset %d out of bounds (len=%d)", offset, len(felts))
	}
	bi := new(big.Int)
	felts[offset].BigInt(bi)

	// Validate range.
	maxVal := new(big.Int).Lsh(big.NewInt(1), uint(bits))
	if bi.Cmp(maxVal) >= 0 {
		return 0, 0, fmt.Errorf("value %s exceeds u%d range", bi.String(), bits)
	}
	return bi.Uint64(), 1, nil
}

// decodeU128 decodes a felt as a u128 string (too large for uint64).
func decodeU128(felts []*felt.Felt, offset int) (val string, consumed int, err error) {
	if offset >= len(felts) {
		return "", 0, fmt.Errorf("offset %d out of bounds (len=%d)", offset, len(felts))
	}
	bi := new(big.Int)
	felts[offset].BigInt(bi)
	return bi.String(), 1, nil
}

// decodeU256 decodes two felts (low, high u128) as a u256 string.
func decodeU256(felts []*felt.Felt, offset int) (val string, consumed int, err error) {
	if offset+1 >= len(felts) {
		return "", 0, fmt.Errorf("u256 requires 2 felts at offset %d (len=%d)", offset, len(felts))
	}
	low := new(big.Int)
	high := new(big.Int)
	felts[offset].BigInt(low)
	felts[offset+1].BigInt(high)

	result := new(big.Int).Lsh(high, 128)
	result.Add(result, low)
	return result.String(), 2, nil
}

// decodeSigned decodes a felt as a signed integer (8-64 bit range -> int64).
func decodeSigned(felts []*felt.Felt, offset, bits int) (val int64, consumed int, err error) {
	if offset >= len(felts) {
		return 0, 0, fmt.Errorf("offset %d out of bounds (len=%d)", offset, len(felts))
	}
	bi := new(big.Int)
	felts[offset].BigInt(bi)

	// Check if value represents a negative number (>= 2^(bits-1)).
	halfRange := new(big.Int).Lsh(big.NewInt(1), uint(bits-1))
	fullRange := new(big.Int).Lsh(big.NewInt(1), uint(bits))
	if bi.Cmp(halfRange) >= 0 {
		bi.Sub(bi, fullRange)
	}
	return bi.Int64(), 1, nil
}

// decodeI128 decodes a felt as a signed i128 string.
func decodeI128(felts []*felt.Felt, offset int) (val string, consumed int, err error) {
	if offset >= len(felts) {
		return "", 0, fmt.Errorf("offset %d out of bounds (len=%d)", offset, len(felts))
	}
	bi := new(big.Int)
	felts[offset].BigInt(bi)

	halfRange := new(big.Int).Lsh(big.NewInt(1), 127)
	fullRange := new(big.Int).Lsh(big.NewInt(1), 128)
	if bi.Cmp(halfRange) >= 0 {
		bi.Sub(bi, fullRange)
	}
	return bi.String(), 1, nil
}

// decodeBool decodes a felt as a boolean.
func decodeBool(felts []*felt.Felt, offset int) (val bool, consumed int, err error) {
	if offset >= len(felts) {
		return false, 0, fmt.Errorf("offset %d out of bounds (len=%d)", offset, len(felts))
	}
	return !felts[offset].IsZero(), 1, nil
}

// decodeByteArray decodes a Cairo ByteArray from felts.
// ByteArray layout: [num_chunks, chunk0, chunk1, ..., pending_word, pending_word_len]
func decodeByteArray(felts []*felt.Felt, offset int) (val string, consumed int, err error) {
	if offset >= len(felts) {
		return "", 0, fmt.Errorf("offset %d out of bounds (len=%d)", offset, len(felts))
	}

	// Read number of full 31-byte chunks.
	numChunks := new(big.Int)
	felts[offset].BigInt(numChunks)
	n := int(numChunks.Int64())
	consumed = 1

	if offset+1+n+2 > len(felts) {
		return "", 0, fmt.Errorf("ByteArray: need %d felts at offset %d (len=%d)", 1+n+2, offset, len(felts))
	}

	var sb strings.Builder

	// Read full 31-byte chunks.
	for i := 0; i < n; i++ {
		chunk := felts[offset+1+i].Bytes()
		// Each chunk is 31 bytes (last 31 bytes of the 32-byte array).
		sb.Write(chunk[1:]) // Skip first byte (always 0 for 31-byte chunks).
	}
	consumed += n

	// Read pending word and its length.
	pendingWord := felts[offset+1+n].Bytes()
	pendingLen := new(big.Int)
	felts[offset+2+n].BigInt(pendingLen)
	pLen := int(pendingLen.Int64())
	consumed += 2

	if pLen > 0 && pLen <= 31 {
		// Extract the last pLen bytes from the pending word.
		start := 32 - pLen
		sb.Write(pendingWord[start:])
	}

	return sb.String(), consumed, nil
}

// decodeArray decodes a length-prefixed Array<T> or Span<T>.
func decodeArray(td *TypeDef, felts []*felt.Felt, offset int) (result []any, consumed int, err error) {
	if offset >= len(felts) {
		return nil, 0, fmt.Errorf("offset %d out of bounds (len=%d)", offset, len(felts))
	}

	// First felt is the array length.
	lenBI := new(big.Int)
	felts[offset].BigInt(lenBI)
	arrLen := int(lenBI.Int64())
	consumed = 1

	if td.Inner == nil {
		return nil, 0, fmt.Errorf("Array/Span has no inner type definition")
	}

	result = make([]any, 0, arrLen)
	for i := 0; i < arrLen; i++ {
		val, n, err := decodeType(td.Inner, felts, offset+consumed)
		if err != nil {
			return nil, 0, fmt.Errorf("decoding array element %d: %w", i, err)
		}
		result = append(result, val)
		consumed += n
	}

	return result, consumed, nil
}

// decodeTuple decodes a tuple by decoding each member in order.
// Members are keyed by their positional index ("0", "1", ...).
func decodeTuple(td *TypeDef, felts []*felt.Felt, offset int) (result map[string]any, consumed int, err error) {
	result = make(map[string]any, len(td.Members))
	consumed = 0

	for _, member := range td.Members {
		val, n, err := decodeType(member.Type, felts, offset+consumed)
		if err != nil {
			return nil, 0, fmt.Errorf("decoding tuple member %q: %w", member.Name, err)
		}
		result[member.Name] = val
		consumed += n
	}

	return result, consumed, nil
}

// decodeStruct decodes a struct by decoding each member in order.
func decodeStruct(td *TypeDef, felts []*felt.Felt, offset int) (result map[string]any, consumed int, err error) {
	result = make(map[string]any, len(td.Members))
	consumed = 0

	for _, member := range td.Members {
		val, n, err := decodeType(member.Type, felts, offset+consumed)
		if err != nil {
			return nil, 0, fmt.Errorf("decoding struct member %q: %w", member.Name, err)
		}
		result[member.Name] = val
		consumed += n
	}

	return result, consumed, nil
}

// decodeEnum decodes an enum value: first felt is the variant index,
// followed by the variant's data.
func decodeEnum(td *TypeDef, felts []*felt.Felt, offset int) (result map[string]any, consumed int, err error) {
	if offset >= len(felts) {
		return nil, 0, fmt.Errorf("offset %d out of bounds (len=%d)", offset, len(felts))
	}

	// First felt is variant index.
	idxBI := new(big.Int)
	felts[offset].BigInt(idxBI)
	idx := int(idxBI.Int64())
	consumed = 1

	if idx >= len(td.Variants) {
		return nil, 0, fmt.Errorf("enum variant index %d out of range (have %d variants)", idx, len(td.Variants))
	}

	variant := td.Variants[idx]
	result = map[string]any{
		"variant": variant.Name,
	}

	// Decode variant data if not unit type.
	if variant.Type.Kind != CairoUnit {
		val, n, err := decodeType(variant.Type, felts, offset+consumed)
		if err != nil {
			return nil, 0, fmt.Errorf("decoding enum variant %q: %w", variant.Name, err)
		}
		result["value"] = val
		consumed += n
	}

	return result, consumed, nil
}
