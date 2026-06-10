package engine

import (
	"testing"

	"github.com/b-j-roberts/ibis/internal/config"
)

func uniqueTbl() config.TableConfig {
	return config.TableConfig{Type: "unique", UniqueKey: "_view_key"}
}

// A reloaded factory child carrying a STALE view set must be re-synced to the
// parent factory's CURRENT child_views/child_events from the YAML.
func TestResyncDynamicChildConfig_ReappliesFactoryViews(t *testing.T) {
	newOrderBookViews := []config.ViewConfig{
		{Function: "get_option_token", Refresh: &config.ViewRefreshConfig{Mode: config.RefreshModeConstant}, Table: uniqueTbl()},
		{Function: "get_depth", Refresh: &config.ViewRefreshConfig{On: []string{"SellOrderPlaced", "OrderFilled", "OrderCancelled"}, Debounce: "1s"}, Table: uniqueTbl()},
		{Function: "get_lowest_sell_price", Refresh: &config.ViewRefreshConfig{On: []string{"SellOrderPlaced", "OrderFilled", "OrderCancelled"}, Debounce: "1s"}, Table: uniqueTbl()},
	}
	childEvents := []config.EventConfig{{Name: "*", Table: config.TableConfig{Type: "log"}}}

	e := &Engine{
		logger: noopLogger(),
		cfg: &config.Config{
			Contracts: []config.ContractConfig{{
				Name:    "OptionManagerBTC",
				Address: "0x1",
				Factories: []config.FactoryConfig{
					{Event: "DeploymentCreated", ChildAddressField: "option_token", ChildABI: "OptionToken",
						ChildViews: []config.ViewConfig{{Function: "get_strike", Refresh: &config.ViewRefreshConfig{Mode: config.RefreshModeConstant}, Table: uniqueTbl()}}, ChildEvents: childEvents},
					{Event: "DeploymentCreated", ChildAddressField: "order_book", ChildABI: "OrderBook",
						ChildViews: newOrderBookViews, ChildEvents: childEvents},
				},
			}},
		},
	}

	// Stale snapshot persisted at first registration: interval polling + a
	// since-removed reserve view.
	child := &config.ContractConfig{
		Name:        "OrderBook_abc123",
		Address:     "0xabc",
		ABI:         "OrderBook",
		FactoryName: "OptionManagerBTC",
		Views: []config.ViewConfig{
			{Function: "get_depth", Interval: "15s", Table: uniqueTbl()},
			{Function: "get_underlying_reserve", Interval: "30s", Table: config.TableConfig{Type: "log"}},
		},
	}

	e.resyncDynamicChildConfig(child)

	if len(child.Views) != len(newOrderBookViews) {
		t.Fatalf("expected %d views after resync, got %d", len(newOrderBookViews), len(child.Views))
	}
	var depth *config.ViewConfig
	for i := range child.Views {
		v := &child.Views[i]
		if v.Function == "get_underlying_reserve" {
			t.Fatal("stale reserve view survived resync")
		}
		if v.Interval != "" {
			t.Fatalf("view %s still has interval after resync", v.Function)
		}
		if v.Function == "get_depth" {
			depth = v
		}
	}
	if depth == nil || depth.Refresh == nil || depth.Refresh.ResolvedMode() != config.RefreshModeReactive {
		t.Fatal("get_depth not reactive after resync")
	}
	// Identity preserved.
	if child.Name != "OrderBook_abc123" || child.Address != "0xabc" || child.FactoryName != "OptionManagerBTC" {
		t.Fatal("child identity mutated by resync")
	}
	// Events re-applied from the factory.
	if len(child.Events) != 1 || child.Events[0].Name != "*" {
		t.Fatalf("events not resynced: %+v", child.Events)
	}
}

// A child whose recorded FactoryName no longer carries a matching factory entry
// (factory config relocated to a shared OptionFactory contract during migration)
// must still resync via the child-ABI fallback.
func TestResyncDynamicChildConfig_FallbackByChildABI(t *testing.T) {
	newViews := []config.ViewConfig{
		{Function: "get_strike", Refresh: &config.ViewRefreshConfig{Mode: config.RefreshModeConstant}, Table: uniqueTbl()},
	}
	childEvents := []config.EventConfig{{Name: "*", Table: config.TableConfig{Type: "log"}}}

	e := &Engine{
		logger: noopLogger(),
		cfg: &config.Config{
			Contracts: []config.ContractConfig{
				// The migrated manager is now a bare entry (no factories block).
				{Name: "OptionManagerETH48B", Address: "0x1", Events: childEvents},
				// Factory config lives on the shared OptionFactory contract.
				{Name: "OptionFactoryETH", Address: "0x2", Events: childEvents, Factories: []config.FactoryConfig{
					{Event: "DeploymentCreated", ChildAddressField: "option_token", ChildABI: "OptionToken",
						ChildViews: newViews, ChildEvents: childEvents, SharedTables: true},
				}},
			},
		},
	}

	// Pre-migration child: name/FactoryName point at the manager, which no longer
	// has a factory entry.
	child := &config.ContractConfig{
		Name:        "OptionManagerETH48B_abc",
		Address:     "0xabc",
		ABI:         "OptionToken",
		FactoryName: "OptionManagerETH48B",
		Views: []config.ViewConfig{
			{Function: "get_underlying_reserve", Interval: "30s", Table: config.TableConfig{Type: "log"}},
		},
	}

	e.resyncDynamicChildConfig(child)

	if len(child.Views) != 1 || child.Views[0].Function != "get_strike" {
		t.Fatalf("fallback did not re-key child to OptionFactoryETH views: %+v", child.Views)
	}
	if child.Views[0].Refresh == nil || child.Views[0].Refresh.ResolvedMode() != config.RefreshModeConstant {
		t.Fatal("fallback view not constant")
	}
}

// No matching parent factory → keep the persisted config (don't break a child
// whose parent was removed from the YAML).
func TestResyncDynamicChildConfig_NoParentMatchKeepsConfig(t *testing.T) {
	e := &Engine{logger: noopLogger(), cfg: &config.Config{}}
	child := &config.ContractConfig{
		Name: "Orphan", ABI: "OrderBook", FactoryName: "MissingParent",
		Views: []config.ViewConfig{{Function: "get_depth", Interval: "15s", Table: uniqueTbl()}},
	}
	e.resyncDynamicChildConfig(child)
	if len(child.Views) != 1 || child.Views[0].Function != "get_depth" || child.Views[0].Interval != "15s" {
		t.Fatalf("expected config untouched when no parent match, got %+v", child.Views)
	}
}

// Non-factory contract (no FactoryName) → no-op.
func TestResyncDynamicChildConfig_NonFactoryNoop(t *testing.T) {
	e := &Engine{logger: noopLogger(), cfg: &config.Config{}}
	child := &config.ContractConfig{Name: "Static", Views: []config.ViewConfig{{Function: "x", Interval: "5m", Table: uniqueTbl()}}}
	e.resyncDynamicChildConfig(child)
	if len(child.Views) != 1 || child.Views[0].Interval != "5m" {
		t.Fatal("non-factory child config should be untouched")
	}
}
