package config

import (
	"encoding/json"
	"testing"
)

const freezeConfig = `
network: mainnet
rpc: wss://starknet-mainnet.example.com
database:
  backend: memory
contracts:
  - name: OptionToken
    address: "0x049d36570d4e46f48e99674bd3fcc84644ddd6b96f7c741b1562b82f9e004dc7"
    abi: OptionToken
    events:
      - name: "*"
        table:
          type: log
    freeze:
      on: [Settled]
      on_foreign:
        - contract: OptionManager
          event: ActiveDeploymentChanged
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
          on: [Settled, Expired]
`

func TestLoad_FreezeConfig(t *testing.T) {
	path := writeTestConfig(t, freezeConfig)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Static contract freeze: local + foreign trigger.
	tok := cfg.Contracts[0]
	if tok.Freeze == nil {
		t.Fatal("OptionToken.Freeze is nil")
	}
	if len(tok.Freeze.On) != 1 || tok.Freeze.On[0] != "Settled" {
		t.Errorf("Freeze.On = %v, want [Settled]", tok.Freeze.On)
	}
	if len(tok.Freeze.OnForeign) != 1 ||
		tok.Freeze.OnForeign[0].Contract != "OptionManager" ||
		tok.Freeze.OnForeign[0].Event != "ActiveDeploymentChanged" {
		t.Errorf("Freeze.OnForeign = %+v, want [{OptionManager ActiveDeploymentChanged}]", tok.Freeze.OnForeign)
	}

	// Factory child freeze.
	fac := cfg.Contracts[1]
	if len(fac.Factories) != 1 {
		t.Fatalf("factories = %d, want 1", len(fac.Factories))
	}
	cf := fac.Factories[0].ChildFreeze
	if cf == nil {
		t.Fatal("Factories[0].ChildFreeze is nil")
	}
	if len(cf.On) != 2 || cf.On[0] != "Settled" || cf.On[1] != "Expired" {
		t.Errorf("ChildFreeze.On = %v, want [Settled Expired]", cf.On)
	}
}

// TestFreezeConfig_JSONRoundTrip guards the persistence path: SaveDynamicContract
// JSON-marshals the contract config and GetDynamicContracts unmarshals it on
// rehydration. The Freeze policy and the runtime Frozen flag must survive intact,
// otherwise a frozen child would re-subscribe (and re-leak RPC) after a restart.
func TestFreezeConfig_JSONRoundTrip(t *testing.T) {
	orig := ContractConfig{
		Name:    "OptionFactoryWBTC_439f969d",
		Address: "0x439f969d",
		ABI:     "OptionToken",
		Dynamic: true,
		Frozen:  true,
		Freeze: &FreezeConfig{
			On:        []string{"Settled"},
			OnForeign: []ForeignTrigger{{Contract: "OptionManager", Event: "ActiveDeploymentChanged"}},
		},
	}

	data, err := json.Marshal(&orig)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var got ContractConfig
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if !got.Frozen {
		t.Error("Frozen flag lost across JSON round-trip")
	}
	if got.Freeze == nil {
		t.Fatal("Freeze lost across JSON round-trip")
	}
	if len(got.Freeze.On) != 1 || got.Freeze.On[0] != "Settled" {
		t.Errorf("Freeze.On = %v, want [Settled]", got.Freeze.On)
	}
	if len(got.Freeze.OnForeign) != 1 || got.Freeze.OnForeign[0].Event != "ActiveDeploymentChanged" {
		t.Errorf("Freeze.OnForeign = %+v", got.Freeze.OnForeign)
	}
}
