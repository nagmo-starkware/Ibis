package config

import (
	"fmt"
	"strings"
)

var validNetworks = map[string]bool{
	"mainnet": true,
	"sepolia": true,
	"custom":  true,
}

var validBackends = map[string]bool{
	"postgres": true,
	"badger":   true,
	"memory":   true,
}

var validTableTypes = map[string]bool{
	"log":         true,
	"unique":      true,
	"aggregation": true,
}

var validAggOps = map[string]bool{
	"sum":   true,
	"count": true,
	"avg":   true,
}

// Validate checks the Config for required fields, valid enum values,
// and contract address format. Returns the first error found.
func Validate(cfg *Config) error {
	if cfg.Network == "" {
		return fieldError("network", "required")
	}
	if !validNetworks[cfg.Network] {
		return fieldError("network", "must be one of: mainnet, sepolia, custom")
	}

	if cfg.RPC == "" {
		return fieldError("rpc", "required")
	}
	scheme := cfg.RPCScheme()
	if scheme != "wss" && scheme != "ws" && scheme != "https" && scheme != "http" {
		return fieldError("rpc", "must use wss://, ws://, https://, or http:// scheme")
	}

	if !validBackends[cfg.Database.Backend] {
		return fieldError("database.backend", "must be one of: postgres, badger, memory")
	}

	if cfg.Database.Backend == "postgres" {
		if cfg.Database.Postgres.Host == "" {
			return fieldError("database.postgres.host", "required when backend is postgres")
		}
		if cfg.Database.Postgres.User == "" {
			return fieldError("database.postgres.user", "required when backend is postgres")
		}
		if cfg.Database.Postgres.Name == "" {
			return fieldError("database.postgres.name", "required when backend is postgres")
		}
	}

	if len(cfg.Contracts) == 0 && len(cfg.Discover) == 0 {
		return fieldError("contracts", "at least one contract or discover entry is required")
	}

	for i := range cfg.Contracts {
		c := &cfg.Contracts[i]
		prefix := fmt.Sprintf("contracts[%d]", i)
		if c.Name == "" {
			return fieldError(prefix+".name", "required")
		}
		if c.Address == "" {
			return fieldError(prefix+".address", "required")
		}
		if err := validateContractAddress(c.Address); err != nil {
			return fieldError(prefix+".address", err.Error())
		}
		if len(c.Events) == 0 {
			return fieldError(prefix+".events", "at least one event is required")
		}
		if err := validateEvents(c.Events, prefix); err != nil {
			return err
		}

		// Validate factory config if present.
		if c.Factory != nil {
			if err := validateFactory(c.Factory, prefix); err != nil {
				return err
			}
		}
	}

	// Validate discover configs.
	if err := validateDiscover(cfg.Discover); err != nil {
		return err
	}

	return nil
}

// validateContractAddress checks that the address looks like a Starknet address.
func validateContractAddress(addr string) error {
	if !strings.HasPrefix(addr, "0x") {
		return fmt.Errorf("must start with 0x")
	}
	hex := addr[2:]
	if hex == "" || len(hex) > 64 {
		return fmt.Errorf("hex part must be 1-64 characters")
	}
	for _, c := range hex {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return fmt.Errorf("invalid hex character: %c", c)
		}
	}
	return nil
}

func validateEvents(events []EventConfig, prefix string) error {
	for j, e := range events {
		ePrefix := fmt.Sprintf("%s.events[%d]", prefix, j)
		if e.Name == "" {
			return fieldError(ePrefix+".name", "required")
		}
		if !validTableTypes[e.Table.Type] {
			return fieldError(ePrefix+".table.type", "must be one of: log, unique, aggregation")
		}
		if e.Table.Type == "unique" && e.Table.UniqueKey == "" {
			return fieldError(ePrefix+".table.unique_key", "required when table type is unique")
		}
		if e.Table.Type == "aggregation" {
			if len(e.Table.Aggregates) == 0 {
				return fieldError(ePrefix+".table.aggregate", "required when table type is aggregation")
			}
			for k, a := range e.Table.Aggregates {
				aPrefix := fmt.Sprintf("%s.table.aggregate[%d]", ePrefix, k)
				if a.Column == "" {
					return fieldError(aPrefix+".column", "required")
				}
				if !validAggOps[a.Operation] {
					return fieldError(aPrefix+".operation", "must be one of: sum, count, avg")
				}
				if a.Field == "" {
					return fieldError(aPrefix+".field", "required")
				}
			}
		}
	}
	return nil
}

func validateFactory(f *FactoryConfig, prefix string) error {
	fPrefix := prefix + ".factory"
	if f.Event == "" {
		return fieldError(fPrefix+".event", "required")
	}
	if f.ChildAddressField == "" {
		return fieldError(fPrefix+".child_address_field", "required")
	}
	if len(f.ChildEvents) == 0 {
		return fieldError(fPrefix+".child_events", "at least one child event is required")
	}
	if err := validateEvents(f.ChildEvents, fPrefix); err != nil {
		return err
	}
	return nil
}

// validateDiscover validates discover config entries for class hash watching.
func validateDiscover(discovers []DiscoverConfig) error {
	seenClassHashes := make(map[string]bool)
	for i := range discovers {
		d := &discovers[i]
		prefix := fmt.Sprintf("discover[%d]", i)

		if d.ClassHash == "" {
			return fieldError(prefix+".class_hash", "required")
		}
		if err := validateHexHash(d.ClassHash); err != nil {
			return fieldError(prefix+".class_hash", err.Error())
		}
		if seenClassHashes[d.ClassHash] {
			return fieldError(prefix+".class_hash", "duplicate class hash")
		}
		seenClassHashes[d.ClassHash] = true

		if d.ABI == "" {
			return fieldError(prefix+".abi", "required")
		}
		if len(d.Events) == 0 {
			return fieldError(prefix+".events", "at least one event is required")
		}
		if err := validateEvents(d.Events, prefix); err != nil {
			return err
		}

		// Validate optional group name: lowercase alphanumeric + hyphens.
		if d.Group != "" {
			for _, c := range d.Group {
				if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-') {
					return fieldError(prefix+".group", "must be lowercase alphanumeric with hyphens only")
				}
			}
		}
	}
	return nil
}

// validateHexHash checks that a string looks like a valid hex hash (0x-prefixed, 1-64 hex chars).
func validateHexHash(hash string) error {
	if !strings.HasPrefix(hash, "0x") {
		return fmt.Errorf("must start with 0x")
	}
	hex := hash[2:]
	if hex == "" || len(hex) > 64 {
		return fmt.Errorf("hex part must be 1-64 characters")
	}
	for _, c := range hex {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return fmt.Errorf("invalid hex character: %c", c)
		}
	}
	return nil
}

func fieldError(field, msg string) error {
	return fmt.Errorf("%s: %s", field, msg)
}
