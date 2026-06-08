package config

import (
	"fmt"
	"strings"
	"time"
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

var validViewTableTypes = map[string]bool{
	"log":    true,
	"unique": true,
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

	if cfg.Indexer.UDCAddress != "" {
		if err := validateHexHash(cfg.Indexer.UDCAddress); err != nil {
			return fieldError("indexer.udc_address", err.Error())
		}
	}

	if cfg.Indexer.UDCEvent != nil {
		if err := validateUDCEvent(cfg.Indexer.UDCEvent); err != nil {
			return err
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

		// Validate each factory config if present.
		for j := range c.Factories {
			fPrefix := fmt.Sprintf("%s.factories[%d]", prefix, j)
			if err := validateFactory(&c.Factories[j], fPrefix); err != nil {
				return err
			}
		}

		// Validate view configs if present.
		if err := validateViews(c.Views, prefix); err != nil {
			return err
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
	if f.Event == "" {
		return fieldError(prefix+".event", "required")
	}
	if f.ChildAddressField == "" {
		return fieldError(prefix+".child_address_field", "required")
	}
	if len(f.ChildEvents) == 0 && len(f.ChildViews) == 0 {
		return fieldError(prefix, "at least one of child_events or child_views is required")
	}
	if err := validateEvents(f.ChildEvents, prefix); err != nil {
		return err
	}
	if err := validateViews(f.ChildViews, prefix); err != nil {
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

		// When shared_tables is true, ABI must be a named value (not "fetch" or a file path)
		// so it can be used as a clean table name prefix.
		if d.SharedTables {
			if d.ABI == "fetch" || isFilePath(d.ABI) {
				return fieldError(prefix+".abi",
					"must be a named ABI (not \"fetch\" or a file path) when shared_tables is true, "+
						"because the ABI name is used as the shared table prefix")
			}
		}

		// Validate view configs if present.
		if err := validateViews(d.Views, prefix); err != nil {
			return err
		}
	}
	return nil
}

// validateViews validates view function configurations.
func validateViews(views []ViewConfig, prefix string) error {
	for j, v := range views {
		vPrefix := fmt.Sprintf("%s.views[%d]", prefix, j)

		if v.Function == "" {
			return fieldError(vPrefix+".function", "required")
		}

		if err := validateViewRefresh(&v, vPrefix); err != nil {
			return err
		}

		if !validViewTableTypes[v.Table.Type] {
			return fieldError(vPrefix+".table.type", "must be one of: log, unique (aggregation not supported for views)")
		}
		if v.Table.Type == "unique" && v.Table.UniqueKey == "" {
			return fieldError(vPrefix+".table.unique_key", "required when table type is unique")
		}

		for k, cd := range v.Calldata {
			cdPrefix := fmt.Sprintf("%s.calldata[%d]", vPrefix, k)
			if err := validateHexFelt(cd); err != nil {
				return fieldError(cdPrefix, err.Error())
			}
		}
	}
	return nil
}

// validateViewRefresh validates the interval/constant/reactive refresh policy
// of a single view. Exactly one mode applies; the rules enforce that the
// fields belonging to other modes are absent.
func validateViewRefresh(v *ViewConfig, vPrefix string) error {
	mode := RefreshModeInterval
	if v.Refresh != nil {
		mode = v.Refresh.ResolvedMode()
	}

	switch mode {
	case RefreshModeInterval:
		// Default mode: Interval is required and must be >= 1s.
		if v.Interval == "" {
			return fieldError(vPrefix+".interval", "required (or set refresh.mode to constant/reactive)")
		}
		d, err := time.ParseDuration(v.Interval)
		if err != nil {
			return fieldError(vPrefix+".interval", fmt.Sprintf("invalid duration: %v", err))
		}
		if d < time.Second {
			return fieldError(vPrefix+".interval", "minimum interval is 1s")
		}

	case RefreshModeConstant:
		if v.Interval != "" {
			return fieldError(vPrefix+".interval", "must be empty when refresh.mode is constant")
		}
		if len(v.Refresh.On) > 0 || len(v.Refresh.OnForeign) > 0 {
			return fieldError(vPrefix+".refresh.on", "must be empty when refresh.mode is constant")
		}

	case RefreshModeReactive:
		if len(v.Refresh.On) == 0 && len(v.Refresh.OnForeign) == 0 {
			return fieldError(vPrefix+".refresh.on", "at least one event is required for reactive refresh")
		}
		for i, ev := range v.Refresh.On {
			if ev == "" {
				return fieldError(fmt.Sprintf("%s.refresh.on[%d]", vPrefix, i), "event name must not be empty")
			}
		}
		for i, f := range v.Refresh.OnForeign {
			fp := fmt.Sprintf("%s.refresh.on_foreign[%d]", vPrefix, i)
			if f.Contract == "" {
				return fieldError(fp+".contract", "required")
			}
			if f.Event == "" {
				return fieldError(fp+".event", "required")
			}
		}
		if v.Refresh.Debounce != "" {
			if _, err := time.ParseDuration(v.Refresh.Debounce); err != nil {
				return fieldError(vPrefix+".refresh.debounce", fmt.Sprintf("invalid duration: %v", err))
			}
		}
		if v.Refresh.MaxInterval != "" {
			d, err := time.ParseDuration(v.Refresh.MaxInterval)
			if err != nil {
				return fieldError(vPrefix+".refresh.max_interval", fmt.Sprintf("invalid duration: %v", err))
			}
			if d < time.Second {
				return fieldError(vPrefix+".refresh.max_interval", "minimum max_interval is 1s")
			}
		}

	default:
		return fieldError(vPrefix+".refresh.mode", fmt.Sprintf("must be one of: constant, reactive (got %q)", mode))
	}

	return nil
}

// validateHexFelt checks that a string is a valid hex felt (0x-prefixed hex string).
func validateHexFelt(s string) error {
	if !strings.HasPrefix(s, "0x") {
		return fmt.Errorf("must start with 0x")
	}
	hex := s[2:]
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

// validateUDCEvent validates the UDCEventFormat configuration.
func validateUDCEvent(u *UDCEventFormat) error {
	prefix := "indexer.udc_event"

	// Validate version.
	validVersions := map[string]bool{"": true, "auto": true, "v0": true, "v1": true}
	if !validVersions[u.Version] {
		return fieldError(prefix+".version", "must be one of: auto, v0, v1")
	}

	// Mutual exclusivity: address_key and address_data cannot both be set.
	if u.AddressKey != nil && u.AddressData != nil {
		return fieldError(prefix, "address_key and address_data are mutually exclusive")
	}
	// Mutual exclusivity: class_hash_key and class_hash_data cannot both be set.
	if u.ClassHashKey != nil && u.ClassHashData != nil {
		return fieldError(prefix, "class_hash_key and class_hash_data are mutually exclusive")
	}

	// Non-negative index values.
	for _, pair := range []struct {
		name string
		val  *int
	}{
		{"address_key", u.AddressKey},
		{"address_data", u.AddressData},
		{"class_hash_key", u.ClassHashKey},
		{"class_hash_data", u.ClassHashData},
	} {
		if pair.val != nil && *pair.val < 0 {
			return fieldError(prefix+"."+pair.name, "must be non-negative")
		}
	}

	// Reject fine-grained overrides when version is explicitly v0 or v1.
	if u.Version == "v0" || u.Version == "v1" {
		hasOverrides := u.AddressKey != nil || u.AddressData != nil ||
			u.ClassHashKey != nil || u.ClassHashData != nil
		if hasOverrides {
			return fieldError(prefix, "fine-grained overrides are not allowed when version is explicitly v0 or v1")
		}
	}

	return nil
}

func fieldError(field, msg string) error {
	return fmt.Errorf("%s: %s", field, msg)
}
