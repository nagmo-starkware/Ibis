package config

import (
	"math/big"
	"testing"
	"time"
)

// ref is a fixed reference time used so now()±dur tests are deterministic.
var ref = time.Date(2026, 6, 27, 12, 0, 0, 0, time.UTC)

func TestEvalPredicate_Ops(t *testing.T) {
	meta := map[string]any{"x": uint64(100)}
	cases := []struct {
		op    string
		value string
		want  bool
	}{
		{"lt", "200", true},
		{"lt", "100", false},
		{"lte", "100", true},
		{"lte", "99", false},
		{"gt", "50", true},
		{"gt", "100", false},
		{"gte", "100", true},
		{"gte", "101", false},
		{"eq", "100", true},
		{"eq", "101", false},
	}
	for _, c := range cases {
		got, err := EvalPredicate(meta, FreezePredicate{MetaField: "x", Op: c.op, Value: c.value}, ref)
		if err != nil {
			t.Fatalf("op %s value %s: unexpected error: %v", c.op, c.value, err)
		}
		if got != c.want {
			t.Errorf("op %s value %s: got %v, want %v", c.op, c.value, got, c.want)
		}
	}
}

func TestEvalPredicate_NowExpiryDirection(t *testing.T) {
	grace := FreezePredicate{MetaField: "expiry", Op: "lt", Value: "now() - 2d"}
	twoDays := int64(2 * 24 * 3600)

	// Expired 3 days ago: expiry < now()-2d -> freeze.
	expired := map[string]any{"expiry": ref.Add(-3 * 24 * time.Hour).Unix()}
	if got, err := EvalPredicate(expired, grace, ref); err != nil || !got {
		t.Errorf("expired 3d ago: got %v err %v, want true", got, err)
	}
	// Expired exactly at the boundary (now()-2d): lt is strict -> not frozen.
	boundary := map[string]any{"expiry": ref.Unix() - twoDays}
	if got, err := EvalPredicate(boundary, grace, ref); err != nil || got {
		t.Errorf("boundary: got %v err %v, want false", got, err)
	}
	// Expired only 1 day ago: still inside grace -> not frozen.
	recent := map[string]any{"expiry": ref.Add(-1 * 24 * time.Hour).Unix()}
	if got, err := EvalPredicate(recent, grace, ref); err != nil || got {
		t.Errorf("expired 1d ago: got %v err %v, want false", got, err)
	}
	// Not yet expired: must never freeze early.
	future := map[string]any{"expiry": ref.Add(24 * time.Hour).Unix()}
	if got, err := EvalPredicate(future, grace, ref); err != nil || got {
		t.Errorf("future expiry: got %v err %v, want false", got, err)
	}
}

func TestEvalPredicate_NowPlusAndUnits(t *testing.T) {
	cases := []struct {
		value string
		want  int64 // expected resolved unix seconds
	}{
		{"now()", ref.Unix()},
		{"now() + 30s", ref.Add(30 * time.Second).Unix()},
		{"now() - 15m", ref.Add(-15 * time.Minute).Unix()},
		{"now() + 2h", ref.Add(2 * time.Hour).Unix()},
		{"now() - 2d", ref.Add(-2 * 24 * time.Hour).Unix()},
	}
	for _, c := range cases {
		got, err := resolvePredicateValue(c.value, ref)
		if err != nil {
			t.Fatalf("%q: unexpected error: %v", c.value, err)
		}
		want := new(big.Float).SetInt64(c.want)
		if got.Cmp(want) != 0 {
			t.Errorf("%q: got %v, want %v", c.value, got, want)
		}
	}
}

func TestEvalPredicate_MetaTypeCoercion(t *testing.T) {
	// expiry arrives as uint64 freshly decoded, and as float64 after a JSON
	// persist/rehydrate round-trip. Both must compare identically. u128/u256 land
	// as decimal strings; felts/addresses as 0x-hex.
	p := FreezePredicate{MetaField: "expiry", Op: "lt", Value: "1000"}
	for _, v := range []any{uint64(500), float64(500), int(500), int64(500), "500", "0x1f4"} {
		got, err := EvalPredicate(map[string]any{"expiry": v}, p, ref)
		if err != nil {
			t.Fatalf("value %T(%v): unexpected error: %v", v, v, err)
		}
		if !got {
			t.Errorf("value %T(%v): got false, want true (500 < 1000)", v, v)
		}
	}
}

func TestEvalPredicate_MissingFieldIsNoMatch(t *testing.T) {
	p := FreezePredicate{MetaField: "expiry", Op: "lt", Value: "now()"}
	got, err := EvalPredicate(map[string]any{"other": uint64(1)}, p, ref)
	if err != nil {
		t.Fatalf("missing field should not error: %v", err)
	}
	if got {
		t.Error("missing field should be no-match (false)")
	}
}

func TestEvalPredicate_BadConfig(t *testing.T) {
	cases := []struct {
		name string
		p    FreezePredicate
	}{
		{"unknown op", FreezePredicate{MetaField: "x", Op: "ne", Value: "1"}},
		{"bad literal", FreezePredicate{MetaField: "x", Op: "lt", Value: "abc"}},
		{"bad now sign", FreezePredicate{MetaField: "x", Op: "lt", Value: "now() 2d"}},
		{"bad duration unit", FreezePredicate{MetaField: "x", Op: "lt", Value: "now() - 2w"}},
		{"empty duration", FreezePredicate{MetaField: "x", Op: "lt", Value: "now() - d"}},
	}
	meta := map[string]any{"x": uint64(1)}
	for _, c := range cases {
		if _, err := EvalPredicate(meta, c.p, ref); err == nil {
			t.Errorf("%s: expected error, got nil", c.name)
		}
	}
}

// TestLoad_DeclarativeFreeze checks the new any:/predicate shape parses and
// normalizes, and that the legacy on:/on_sibling forms still load unchanged.
func TestLoad_DeclarativeFreeze(t *testing.T) {
	const cfgYAML = `
network: mainnet
rpc: wss://example.com
database:
  backend: memory
contracts:
  - name: OptionFactory
    address: "0x07ef1a171332433c07e36fc5b1d6609ccb3f2fd4243f8a2bc7cce586c0567245"
    abi: OptionFactory
    events:
      - name: "*"
        table:
          type: log
    factories:
      - event: DeploymentCreated
        child_address_field: option_token
        child_abi: OptionToken
        shared_tables: true
        child_events:
          - name: "*"
            table:
              type: log
        child_freeze:
          any:
            - event: Settled
            - predicate:
                meta_field: expiry
                op: lt
                value: "now() - 2d"
`
	cfg, err := Load(writeTestConfig(t, cfgYAML))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	cf := cfg.Contracts[0].Factories[0].ChildFreeze
	if cf == nil {
		t.Fatal("ChildFreeze nil")
	}
	if got := cf.LocalEvents(); len(got) != 1 || got[0] != "Settled" {
		t.Errorf("LocalEvents() = %v, want [Settled]", got)
	}
	preds := cf.Predicates()
	if len(preds) != 1 || preds[0].MetaField != "expiry" || preds[0].Op != "lt" || preds[0].Value != "now() - 2d" {
		t.Errorf("Predicates() = %+v, want one expiry/lt/now()-2d", preds)
	}
}

// TestLoad_DeclarativeFreeze_Rejects guards that a malformed predicate fails
// validation at load time rather than silently never firing.
func TestLoad_DeclarativeFreeze_Rejects(t *testing.T) {
	const cfgYAML = `
network: mainnet
rpc: wss://example.com
database:
  backend: memory
contracts:
  - name: Opt
    address: "0x07ef1a171332433c07e36fc5b1d6609ccb3f2fd4243f8a2bc7cce586c0567245"
    abi: OptionToken
    events:
      - name: "*"
        table:
          type: log
    freeze:
      any:
        - predicate:
            meta_field: expiry
            op: between
            value: "1"
`
	if _, err := Load(writeTestConfig(t, cfgYAML)); err == nil {
		t.Fatal("expected load to reject predicate with unknown op")
	}
}

func TestFreezePredicate_Validate(t *testing.T) {
	if err := (FreezePredicate{MetaField: "expiry", Op: "lt", Value: "now() - 2d"}).Validate(); err != nil {
		t.Errorf("valid predicate rejected: %v", err)
	}
	bad := []FreezePredicate{
		{MetaField: "", Op: "lt", Value: "1"},        // missing field
		{MetaField: "x", Op: "between", Value: "1"},  // bad op
		{MetaField: "x", Op: "lt", Value: "now()?"},  // bad value
		{MetaField: "x", Op: "lt", Value: "now()-2x"}, // bad unit
	}
	for i, p := range bad {
		if err := p.Validate(); err == nil {
			t.Errorf("bad predicate %d (%+v): expected validation error", i, p)
		}
	}
}
