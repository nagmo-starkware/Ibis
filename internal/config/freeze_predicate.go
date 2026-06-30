package config

import (
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"time"
)

// freezeOps is the set of comparison operators a FreezePredicate may use.
var freezeOps = map[string]bool{"lt": true, "gt": true, "lte": true, "gte": true, "eq": true}

// Validate reports a configuration error if the predicate is malformed: missing
// meta_field, an unknown op, or a value that is neither a numeric literal nor a
// well-formed now()±<duration> expression. The duration grammar is validated
// against a fixed reference time, so a bad unit/format is caught at load.
func (p FreezePredicate) Validate() error {
	if strings.TrimSpace(p.MetaField) == "" {
		return fmt.Errorf("freeze predicate: meta_field is required")
	}
	if !freezeOps[p.Op] {
		return fmt.Errorf("freeze predicate: unknown op %q (want lt|gt|lte|gte|eq)", p.Op)
	}
	if _, err := resolvePredicateValue(p.Value, time.Unix(0, 0)); err != nil {
		return err
	}
	return nil
}

// EvalPredicate reports whether meta[p.MetaField] <op> value holds at time now.
//
// The right-hand side is p.Value resolved at now (a literal, or now()±<dur>).
// The left-hand side is the captured meta value coerced to a number. A missing
// or nil meta field is NOT an error — it yields false so a child that never
// captured the field simply stays live rather than crashing the indexer; an
// unknown op or unparseable value/meta is a real error and is returned.
func EvalPredicate(meta map[string]any, p FreezePredicate, now time.Time) (bool, error) {
	if !freezeOps[p.Op] {
		return false, fmt.Errorf("freeze predicate: unknown op %q (want lt|gt|lte|gte|eq)", p.Op)
	}
	rhs, err := resolvePredicateValue(p.Value, now)
	if err != nil {
		return false, err
	}
	raw, ok := meta[p.MetaField]
	if !ok || raw == nil {
		return false, nil
	}
	lhs, err := metaToFloat(raw)
	if err != nil {
		return false, fmt.Errorf("freeze predicate: meta_field %q: %w", p.MetaField, err)
	}
	cmp := lhs.Cmp(rhs)
	switch p.Op {
	case "lt":
		return cmp < 0, nil
	case "lte":
		return cmp <= 0, nil
	case "gt":
		return cmp > 0, nil
	case "gte":
		return cmp >= 0, nil
	case "eq":
		return cmp == 0, nil
	default:
		return false, fmt.Errorf("freeze predicate: unknown op %q", p.Op)
	}
}

// resolvePredicateValue resolves a predicate's value string to a number. It is
// either a numeric literal (decimal or 0x-hex) or a time expression
// "now() [+|-] <duration>" resolving to a unix-seconds count at now.
func resolvePredicateValue(raw string, now time.Time) (*big.Float, error) {
	s := strings.TrimSpace(raw)
	if rest, isNow := strings.CutPrefix(s, "now()"); isNow {
		rest = strings.TrimSpace(rest)
		if rest == "" {
			return new(big.Float).SetInt64(now.Unix()), nil
		}
		sign := rest[0]
		if sign != '+' && sign != '-' {
			return nil, fmt.Errorf("freeze predicate: expected + or - after now() in %q", raw)
		}
		dur, err := parseFreezeDuration(strings.TrimSpace(rest[1:]))
		if err != nil {
			return nil, err
		}
		if sign == '-' {
			dur = -dur
		}
		return new(big.Float).SetInt64(now.Add(dur).Unix()), nil
	}
	return parseNumeric(s)
}

// parseFreezeDuration parses a single-unit duration of the form <int><unit> with
// unit in s|m|h|d. Go's time.ParseDuration is not used because it has no day
// unit. Single-unit is sufficient for freeze grace windows; if a compound form
// (e.g. "1d12h") is ever needed, extend here.
func parseFreezeDuration(s string) (time.Duration, error) {
	if len(s) < 2 {
		return 0, fmt.Errorf("freeze predicate: invalid duration %q (want <int><s|m|h|d>)", s)
	}
	n, err := strconv.ParseInt(s[:len(s)-1], 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("freeze predicate: invalid duration %q (want <int><s|m|h|d>)", s)
	}
	switch s[len(s)-1] {
	case 's':
		return time.Duration(n) * time.Second, nil
	case 'm':
		return time.Duration(n) * time.Minute, nil
	case 'h':
		return time.Duration(n) * time.Hour, nil
	case 'd':
		return time.Duration(n) * 24 * time.Hour, nil
	default:
		return 0, fmt.Errorf("freeze predicate: invalid duration unit in %q (want s|m|h|d)", s)
	}
}

// metaToFloat coerces a captured factory_meta value to a number. Meta values are
// any: freshly-decoded integers arrive as uint64; after the JSON persist/rehydrate
// round-trip the same value is a float64; large Cairo ints (u128/u256) are
// decimal strings; addresses/felts stringify to 0x-hex. All are handled.
func metaToFloat(v any) (*big.Float, error) {
	switch x := v.(type) {
	case float64:
		return big.NewFloat(x), nil
	case float32:
		return big.NewFloat(float64(x)), nil
	case int:
		return new(big.Float).SetInt64(int64(x)), nil
	case int64:
		return new(big.Float).SetInt64(x), nil
	case uint64:
		return new(big.Float).SetUint64(x), nil
	case uint:
		return new(big.Float).SetUint64(uint64(x)), nil
	case *big.Int:
		return new(big.Float).SetInt(x), nil
	case string:
		return parseNumeric(x)
	case fmt.Stringer:
		return parseNumeric(x.String())
	default:
		return parseNumeric(fmt.Sprintf("%v", v))
	}
}

// parseNumeric parses a decimal or 0x-hex numeric string into a big.Float.
func parseNumeric(s string) (*big.Float, error) {
	s = strings.TrimSpace(s)
	if h, ok := strings.CutPrefix(s, "0x"); ok {
		bi, ok := new(big.Int).SetString(h, 16)
		if !ok {
			return nil, fmt.Errorf("freeze predicate: %q is not a valid hex number", s)
		}
		return new(big.Float).SetInt(bi), nil
	}
	f, _, err := big.ParseFloat(s, 10, 256, big.ToNearestEven)
	if err != nil {
		return nil, fmt.Errorf("freeze predicate: %q is not a number or now()±<dur>", s)
	}
	return f, nil
}
