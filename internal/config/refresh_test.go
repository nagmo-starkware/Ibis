package config

import (
	"testing"

	"gopkg.in/yaml.v3"
)

// --- YAML parsing of the refresh field ---

func TestViewRefresh_ParsesReactiveMapping(t *testing.T) {
	var v ViewConfig
	src := `
function: get_depth
refresh:
  on: [OrderFilled, OrderCancelled]
  debounce: 1s
  max_interval: 6h
table:
  type: unique
  unique_key: _view_key
`
	if err := yaml.Unmarshal([]byte(src), &v); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if v.Interval != "" {
		t.Fatalf("expected empty interval, got %q", v.Interval)
	}
	if v.Refresh == nil {
		t.Fatal("expected refresh to be set")
	}
	if got := v.Refresh.ResolvedMode(); got != RefreshModeReactive {
		t.Fatalf("expected reactive mode, got %q", got)
	}
	if len(v.Refresh.On) != 2 || v.Refresh.On[0] != "OrderFilled" || v.Refresh.On[1] != "OrderCancelled" {
		t.Fatalf("unexpected on list: %v", v.Refresh.On)
	}
	if v.Refresh.Debounce != "1s" {
		t.Fatalf("expected debounce 1s, got %q", v.Refresh.Debounce)
	}
	if v.Refresh.MaxInterval != "6h" {
		t.Fatalf("expected max_interval 6h, got %q", v.Refresh.MaxInterval)
	}
}

func TestViewRefresh_ParsesConstantScalarShorthand(t *testing.T) {
	var v ViewConfig
	src := `
function: get_strike
refresh: constant
table:
  type: unique
  unique_key: _view_key
`
	if err := yaml.Unmarshal([]byte(src), &v); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if v.Refresh == nil {
		t.Fatal("expected refresh to be set")
	}
	if got := v.Refresh.ResolvedMode(); got != RefreshModeConstant {
		t.Fatalf("expected constant mode, got %q", got)
	}
	if len(v.Refresh.On) != 0 {
		t.Fatalf("expected no on events, got %v", v.Refresh.On)
	}
}

func TestViewRefresh_ParsesForeignTriggers(t *testing.T) {
	var v ViewConfig
	src := `
function: get_price_scale
refresh:
  on_foreign:
    - { contract: Oracle, event: PriceUpdated }
table:
  type: unique
  unique_key: _view_key
`
	if err := yaml.Unmarshal([]byte(src), &v); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := v.Refresh.ResolvedMode(); got != RefreshModeReactive {
		t.Fatalf("expected reactive (inferred from on_foreign), got %q", got)
	}
	if len(v.Refresh.OnForeign) != 1 || v.Refresh.OnForeign[0].Contract != "Oracle" || v.Refresh.OnForeign[0].Event != "PriceUpdated" {
		t.Fatalf("unexpected on_foreign: %+v", v.Refresh.OnForeign)
	}
}

// --- Validation of the three refresh modes ---

// cfgWithView wraps a single view in an otherwise-valid config.
func cfgWithView(v ViewConfig) *Config {
	return &Config{
		Network:  "mainnet",
		RPC:      "wss://example.com",
		Database: DatabaseConfig{Backend: "memory"},
		Contracts: []ContractConfig{{
			Name:    "OB",
			Address: "0x049d36570d4e46f48e99674bd3fcc84644ddd6b96f7c741b1562b82f9e004dc7",
			Events:  []EventConfig{{Name: "*", Table: TableConfig{Type: "log"}}},
			Views:   []ViewConfig{v},
		}},
	}
}

func uniqueTable() TableConfig { return TableConfig{Type: "unique", UniqueKey: "_view_key"} }

func TestValidate_Refresh(t *testing.T) {
	tests := []struct {
		name    string
		view    ViewConfig
		wantErr bool
	}{
		{
			name: "interval mode valid",
			view: ViewConfig{Function: "f", Interval: "30s", Table: uniqueTable()},
		},
		{
			name:    "interval mode missing interval",
			view:    ViewConfig{Function: "f", Table: uniqueTable()},
			wantErr: true,
		},
		{
			name:    "interval below minimum",
			view:    ViewConfig{Function: "f", Interval: "500ms", Table: uniqueTable()},
			wantErr: true,
		},
		{
			name: "constant valid",
			view: ViewConfig{Function: "f", Refresh: &ViewRefreshConfig{Mode: RefreshModeConstant}, Table: uniqueTable()},
		},
		{
			name:    "constant must not set interval",
			view:    ViewConfig{Function: "f", Interval: "30s", Refresh: &ViewRefreshConfig{Mode: RefreshModeConstant}, Table: uniqueTable()},
			wantErr: true,
		},
		{
			name:    "constant must not set on",
			view:    ViewConfig{Function: "f", Refresh: &ViewRefreshConfig{Mode: RefreshModeConstant, On: []string{"E"}}, Table: uniqueTable()},
			wantErr: true,
		},
		{
			name: "reactive valid",
			view: ViewConfig{Function: "f", Refresh: &ViewRefreshConfig{On: []string{"OrderFilled"}, Debounce: "1s", MaxInterval: "6h"}, Table: uniqueTable()},
		},
		{
			name:    "reactive requires on or on_foreign",
			view:    ViewConfig{Function: "f", Refresh: &ViewRefreshConfig{Mode: RefreshModeReactive}, Table: uniqueTable()},
			wantErr: true,
		},
		{
			name:    "reactive bad debounce",
			view:    ViewConfig{Function: "f", Refresh: &ViewRefreshConfig{On: []string{"E"}, Debounce: "nope"}, Table: uniqueTable()},
			wantErr: true,
		},
		{
			name:    "reactive max_interval below minimum",
			view:    ViewConfig{Function: "f", Refresh: &ViewRefreshConfig{On: []string{"E"}, MaxInterval: "100ms"}, Table: uniqueTable()},
			wantErr: true,
		},
		{
			name:    "unknown mode",
			view:    ViewConfig{Function: "f", Refresh: &ViewRefreshConfig{Mode: "bogus"}, Table: uniqueTable()},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Validate(cfgWithView(tt.view))
			if tt.wantErr && err == nil {
				t.Fatal("expected validation error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("unexpected validation error: %v", err)
			}
		})
	}
}
