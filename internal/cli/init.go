package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/b-j-roberts/ibis/internal/abi"
	"github.com/b-j-roberts/ibis/internal/config"
	"github.com/b-j-roberts/ibis/internal/provider"
)

var (
	initContracts      []string
	initNames          []string
	initOutput         string
	initNetwork        string
	initRPC            string
	initDatabase       string
	initNonInteractive bool
)

var initCmd = &cobra.Command{
	Use:   "init",
	Short: "Scaffold an ibis.config.yaml from contract inspection",
	Long: `Scaffolds an ibis.config.yaml by inspecting contracts on-chain.

Interactive mode (default):
  ibis init --contract 0x049d36...

Non-interactive mode (for CI/scripting):
  ibis init --contract 0x049d36... --name MyToken --network mainnet --rpc wss://... --database memory --non-interactive`,
	RunE: runInit,
}

func init() {
	initCmd.Flags().StringSliceVar(&initContracts, "contract", nil, "contract address(es) to index (can be specified multiple times)")
	initCmd.Flags().StringSliceVar(&initNames, "name", nil, "contract name(s), applied in order to --contract addresses")
	initCmd.Flags().StringVar(&initOutput, "output", "./ibis.config.yaml", "output path for generated config")
	initCmd.Flags().StringVar(&initNetwork, "network", "", "network: mainnet, sepolia, or custom")
	initCmd.Flags().StringVar(&initRPC, "rpc", "", "RPC endpoint URL (WSS or HTTP)")
	initCmd.Flags().StringVar(&initDatabase, "database", "", "database backend: memory, badger, or postgres")
	initCmd.Flags().BoolVar(&initNonInteractive, "non-interactive", false, "skip interactive prompts, use flag values")
}

// defaultRPCURLs maps network names to default public RPC endpoints.
var defaultRPCURLs = map[string]string{
	"mainnet": "https://rpc.starknet.lava.build",
	"sepolia": "https://rpc.starknet-sepolia.lava.build",
}

func runInit(cmd *cobra.Command, args []string) error {
	out := cmd.OutOrStdout()
	p := newPrompter(os.Stdin, out)

	fmt.Fprintln(out, "Ibis Config Generator")
	fmt.Fprintln(out, strings.Repeat("=", 40))

	// Step 1: Network selection.
	network, err := resolveNetwork(p)
	if err != nil {
		return err
	}

	// Step 2: RPC URL.
	rpcURL, err := resolveRPC(p, network)
	if err != nil {
		return err
	}

	// Step 3: Database backend.
	database, err := resolveDatabase(p)
	if err != nil {
		return err
	}

	// Step 4: Contract addresses.
	contracts, err := resolveContracts(p)
	if err != nil {
		return err
	}

	// Validate --name count matches --contract count when provided.
	if len(initNames) > 0 && len(initNames) != len(contracts) {
		return fmt.Errorf("--name count (%d) must match --contract count (%d)", len(initNames), len(contracts))
	}

	// Step 5: Fetch ABIs and configure events for each contract.
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelWarn,
	}))

	ctx := cmd.Context()
	prov, err := provider.New(ctx, rpcURL, logger)
	if err != nil {
		return fmt.Errorf("connecting to RPC: %w", err)
	}
	defer prov.Close()

	resolver := config.NewABIResolver(prov)

	var contractConfigs []config.ContractConfig
	for i, addr := range contracts {
		var nameOverride string
		if i < len(initNames) {
			nameOverride = initNames[i]
		}
		cc, err := configureContract(ctx, p, resolver, addr, nameOverride)
		if err != nil {
			return fmt.Errorf("configuring contract %s: %w", addr, err)
		}
		contractConfigs = append(contractConfigs, cc)
	}

	// Step 6: Build and write config.
	cfg := buildConfig(network, rpcURL, database, contractConfigs)
	return writeConfig(out, cfg, initOutput)
}

func resolveNetwork(p *prompter) (string, error) {
	if initNonInteractive || initNetwork != "" {
		if initNetwork == "" {
			initNetwork = "mainnet"
		}
		return initNetwork, nil
	}

	networks := []string{"mainnet", "sepolia", "custom"}
	idx, err := p.selectOne("Select network", networks, 0)
	if err != nil {
		return "", err
	}
	return networks[idx], nil
}

func resolveRPC(p *prompter, network string) (string, error) {
	if initNonInteractive || initRPC != "" {
		if initRPC == "" {
			if url, ok := defaultRPCURLs[network]; ok {
				return url, nil
			}
			return "", fmt.Errorf("--rpc is required for network %q in non-interactive mode", network)
		}
		return initRPC, nil
	}

	defaultURL := defaultRPCURLs[network]
	hint := "WSS preferred for real-time subscriptions"
	if defaultURL != "" {
		hint = fmt.Sprintf("default: %s", defaultURL)
	}
	fmt.Fprintf(p.out, "\n(%s)\n", hint)

	url, err := p.input("RPC endpoint URL", defaultURL)
	if err != nil {
		return "", err
	}
	if url == "" {
		return "", fmt.Errorf("RPC URL is required")
	}
	return url, nil
}

func resolveDatabase(p *prompter) (string, error) {
	if initNonInteractive || initDatabase != "" {
		if initDatabase == "" {
			initDatabase = "memory"
		}
		return initDatabase, nil
	}

	backends := []string{"memory (no setup, data lost on restart)", "badger (embedded, persists to disk)", "postgres (production-grade)"}
	idx, err := p.selectOne("Select database backend", backends, 0)
	if err != nil {
		return "", err
	}
	names := []string{"memory", "badger", "postgres"}
	return names[idx], nil
}

func resolveContracts(p *prompter) ([]string, error) {
	if len(initContracts) > 0 {
		return initContracts, nil
	}

	if initNonInteractive {
		return nil, fmt.Errorf("--contract is required in non-interactive mode")
	}

	var contracts []string
	for {
		addr, err := p.input("\nContract address (0x...)", "")
		if err != nil {
			return nil, err
		}
		if addr == "" {
			if len(contracts) == 0 {
				fmt.Fprintln(p.out, "At least one contract address is required.")
				continue
			}
			break
		}
		if !strings.HasPrefix(addr, "0x") {
			fmt.Fprintln(p.out, "Address must start with 0x")
			continue
		}
		contracts = append(contracts, addr)

		more, err := p.confirm("Add another contract?", false)
		if err != nil {
			return nil, err
		}
		if !more {
			break
		}
	}
	return contracts, nil
}

// configureContract fetches the ABI for a contract and prompts for event configuration.
// If nameOverride is non-empty, it is used as the contract name without prompting.
func configureContract(ctx context.Context, p *prompter, resolver *config.ABIResolver, address, nameOverride string) (config.ContractConfig, error) {
	cc := config.ContractConfig{
		Address: address,
		ABI:     "fetch",
	}

	// Prompt for contract name.
	switch {
	case nameOverride != "":
		cc.Name = nameOverride
	case !initNonInteractive:
		defaultName := shortContractName(address)
		name, err := p.input(fmt.Sprintf("\nName for contract %s", truncateAddress(address)), defaultName)
		if err != nil {
			return cc, err
		}
		cc.Name = name
	default:
		cc.Name = shortContractName(address)
	}

	// Fetch ABI from chain.
	fmt.Fprintf(p.out, "Fetching ABI for %s from chain...\n", truncateAddress(address))
	parsed, err := resolver.Resolve(ctx, &cc)
	if err != nil {
		return cc, fmt.Errorf("fetching ABI: %w", err)
	}

	if len(parsed.Events) == 0 {
		fmt.Fprintln(p.out, "  No events found in contract ABI.")
		return cc, nil
	}

	fmt.Fprintf(p.out, "  Found %d events:\n", len(parsed.Events))
	for _, ev := range parsed.Events {
		fields := describeEventFields(ev)
		fmt.Fprintf(p.out, "    - %s (%s)\n", ev.Name, fields)
	}

	// Select which events to index.
	events, err := selectEvents(p, parsed)
	if err != nil {
		return cc, err
	}
	cc.Events = events

	return cc, nil
}

// selectEvents prompts the user to pick events and configure their table types.
func selectEvents(p *prompter, parsed *abi.ABI) ([]config.EventConfig, error) {
	if initNonInteractive {
		// In non-interactive mode, index all events as log tables.
		return []config.EventConfig{{
			Name:  "*",
			Table: config.TableConfig{Type: "log"},
		}}, nil
	}

	// Ask if user wants to index all events.
	useWildcard, err := p.confirm("\nIndex all events with wildcard (*)?", true)
	if err != nil {
		return nil, err
	}

	if useWildcard {
		events := []config.EventConfig{{
			Name:  "*",
			Table: config.TableConfig{Type: "log"},
		}}

		// Ask if any specific events need custom table types.
		customize, err := p.confirm("Customize table type for specific events?", false)
		if err != nil {
			return nil, err
		}
		if customize {
			overrides, err := configureEventOverrides(p, parsed)
			if err != nil {
				return nil, err
			}
			events = append(events, overrides...)
		}
		return events, nil
	}

	// Manual event selection.
	eventNames := make([]string, len(parsed.Events))
	for i, ev := range parsed.Events {
		eventNames[i] = ev.Name
	}

	indices, err := p.selectMulti("Select events to index", eventNames)
	if err != nil {
		return nil, err
	}

	var events []config.EventConfig
	for _, idx := range indices {
		ev := parsed.Events[idx]
		ec, err := configureEventTable(p, ev)
		if err != nil {
			return nil, err
		}
		events = append(events, ec)
	}
	return events, nil
}

// configureEventOverrides prompts for specific event overrides when using wildcard.
func configureEventOverrides(p *prompter, parsed *abi.ABI) ([]config.EventConfig, error) {
	var overrides []config.EventConfig
	for {
		eventNames := make([]string, len(parsed.Events))
		for i, ev := range parsed.Events {
			eventNames[i] = ev.Name
		}

		idx, err := p.selectOne("Select event to customize", eventNames, 0)
		if err != nil {
			return nil, err
		}

		ev := parsed.Events[idx]
		ec, err := configureEventTable(p, ev)
		if err != nil {
			return nil, err
		}
		overrides = append(overrides, ec)

		more, err := p.confirm("Customize another event?", false)
		if err != nil {
			return nil, err
		}
		if !more {
			break
		}
	}
	return overrides, nil
}

// configureEventTable prompts for the table type and related settings for an event.
func configureEventTable(p *prompter, ev *abi.EventDef) (config.EventConfig, error) {
	ec := config.EventConfig{Name: ev.Name}

	tableTypes := []string{"log (append-only event log)", "unique (last-write-wins by key)", "aggregation (auto-computed aggregates)"}
	idx, err := p.selectOne(fmt.Sprintf("Table type for %s", ev.Name), tableTypes, 0)
	if err != nil {
		return ec, err
	}

	typeNames := []string{"log", "unique", "aggregation"}
	ec.Table.Type = typeNames[idx]

	switch ec.Table.Type {
	case "unique":
		ec.Table, err = configureUniqueTable(p, ev, ec.Table)
		if err != nil {
			return ec, err
		}
	case "aggregation":
		ec.Table, err = configureAggregationTable(p, ev, ec.Table)
		if err != nil {
			return ec, err
		}
	}

	return ec, nil
}

// configureUniqueTable prompts for the unique key field.
func configureUniqueTable(p *prompter, ev *abi.EventDef, tc config.TableConfig) (config.TableConfig, error) {
	fields := allEventFieldNames(ev)
	if len(fields) == 0 {
		return tc, fmt.Errorf("event %s has no fields to use as unique key", ev.Name)
	}

	idx, err := p.selectOne(fmt.Sprintf("Unique key field for %s", ev.Name), fields, 0)
	if err != nil {
		return tc, err
	}
	tc.UniqueKey = fields[idx]
	return tc, nil
}

// configureAggregationTable prompts for aggregate fields and operations.
func configureAggregationTable(p *prompter, ev *abi.EventDef, tc config.TableConfig) (config.TableConfig, error) {
	fields := allEventFieldNames(ev)
	if len(fields) == 0 {
		return tc, fmt.Errorf("event %s has no fields to aggregate", ev.Name)
	}

	for {
		fieldIdx, err := p.selectOne("Field to aggregate", fields, 0)
		if err != nil {
			return tc, err
		}
		field := fields[fieldIdx]

		ops := []string{"sum", "count", "avg"}
		opIdx, err := p.selectOne(fmt.Sprintf("Aggregation operation for %s", field), ops, 0)
		if err != nil {
			return tc, err
		}

		columnName, err := p.input("Column name for aggregate result", fmt.Sprintf("%s_%s", ops[opIdx], field))
		if err != nil {
			return tc, err
		}

		tc.Aggregates = append(tc.Aggregates, config.AggregateConfig{
			Column:    columnName,
			Operation: ops[opIdx],
			Field:     field,
		})

		more, err := p.confirm("Add another aggregate?", false)
		if err != nil {
			return tc, err
		}
		if !more {
			break
		}
	}

	return tc, nil
}

// buildConfig assembles the final Config from collected values.
func buildConfig(network, rpcURL, database string, contracts []config.ContractConfig) *config.Config {
	cfg := &config.Config{
		Network:   network,
		RPC:       rpcURL,
		Contracts: contracts,
	}

	cfg.Database.Backend = database
	switch database {
	case "postgres":
		cfg.Database.Postgres = config.PostgresConfig{
			Host:     "${IBIS_DB_HOST}",
			Port:     5432,
			User:     "${IBIS_DB_USER}",
			Password: "${IBIS_DB_PASSWORD}",
			Name:     "${IBIS_DB_NAME}",
		}
	case "badger":
		cfg.Database.Badger = config.BadgerConfig{
			Path: "./data/ibis",
		}
	}

	cfg.API.Host = "0.0.0.0"
	cfg.API.Port = 8080

	// StartBlock left nil so the indexer starts from the chain tip by default.
	// Users can set start_block in the config to backfill from a specific block.
	cfg.Indexer.PendingBlocks = true
	cfg.Indexer.BatchSize = 10

	return cfg
}

// writeConfig marshals the config to a clean YAML and writes it to the output path.
// Only includes non-empty sections to keep the output readable.
func writeConfig(out io.Writer, cfg *config.Config, path string) error {
	clean := buildCleanYAML(cfg)

	var yamlBuf bytes.Buffer
	enc := yaml.NewEncoder(&yamlBuf)
	enc.SetIndent(2)
	if err := enc.Encode(&clean); err != nil {
		return fmt.Errorf("marshaling config: %w", err)
	}
	enc.Close()
	data := yamlBuf.Bytes()

	header := "# ibis.config.yaml - Generated by `ibis init`\n" +
		"#\n" +
		"# Environment variables can be referenced with ${VAR_NAME} syntax.\n" +
		"# Run `ibis run` to start indexing with this config.\n\n"

	content := []byte(header + string(data))

	if err := os.WriteFile(path, content, 0o644); err != nil {
		return fmt.Errorf("writing config to %s: %w", path, err)
	}

	fmt.Fprintf(out, "\nConfig written to %s\n", path)
	fmt.Fprintf(out, "Run `ibis run --config %s` to start indexing.\n", path)
	return nil
}

// buildCleanYAML creates an ordered map structure that omits zero/empty values.
func buildCleanYAML(cfg *config.Config) yaml.Node {
	root := yaml.Node{Kind: yaml.MappingNode}

	addKV := func(key string, value *yaml.Node) {
		root.Content = append(root.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Value: key},
			value,
		)
	}
	scalar := func(v string) *yaml.Node {
		return &yaml.Node{Kind: yaml.ScalarNode, Value: v}
	}

	addKV("network", scalar(cfg.Network))
	addKV("rpc", scalar(cfg.RPC))

	// Database -- only include the active backend section.
	db := &yaml.Node{Kind: yaml.MappingNode}
	db.Content = append(db.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Value: "backend"},
		scalar(cfg.Database.Backend),
	)
	switch cfg.Database.Backend {
	case "postgres":
		pg := &yaml.Node{Kind: yaml.MappingNode}
		pg.Content = append(pg.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Value: "host"}, scalar(cfg.Database.Postgres.Host),
			&yaml.Node{Kind: yaml.ScalarNode, Value: "port"}, scalar(fmt.Sprintf("%d", cfg.Database.Postgres.Port)),
			&yaml.Node{Kind: yaml.ScalarNode, Value: "user"}, scalar(cfg.Database.Postgres.User),
			&yaml.Node{Kind: yaml.ScalarNode, Value: "password"}, scalar(cfg.Database.Postgres.Password),
			&yaml.Node{Kind: yaml.ScalarNode, Value: "name"}, scalar(cfg.Database.Postgres.Name),
		)
		db.Content = append(db.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Value: "postgres"}, pg,
		)
	case "badger":
		bg := &yaml.Node{Kind: yaml.MappingNode}
		bg.Content = append(bg.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Value: "path"}, scalar(cfg.Database.Badger.Path),
		)
		db.Content = append(db.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Value: "badger"}, bg,
		)
	}
	addKV("database", db)

	// API.
	apiNode := &yaml.Node{Kind: yaml.MappingNode}
	apiNode.Content = append(apiNode.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Value: "host"}, scalar(cfg.API.Host),
		&yaml.Node{Kind: yaml.ScalarNode, Value: "port"}, scalar(fmt.Sprintf("%d", cfg.API.Port)),
	)
	addKV("api", apiNode)

	// Indexer.
	idx := &yaml.Node{Kind: yaml.MappingNode}
	if cfg.Indexer.StartBlock != nil {
		idx.Content = append(idx.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Value: "start_block"}, scalar(fmt.Sprintf("%d", *cfg.Indexer.StartBlock)),
		)
	}
	idx.Content = append(idx.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Value: "pending_blocks"}, scalar(fmt.Sprintf("%t", cfg.Indexer.PendingBlocks)),
		&yaml.Node{Kind: yaml.ScalarNode, Value: "batch_size"}, scalar(fmt.Sprintf("%d", cfg.Indexer.BatchSize)),
	)
	addKV("indexer", idx)

	// Contracts.
	contractsSeq := &yaml.Node{Kind: yaml.SequenceNode}
	for i := range cfg.Contracts {
		c := &cfg.Contracts[i]
		cc := &yaml.Node{Kind: yaml.MappingNode}
		cc.Content = append(cc.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Value: "name"}, scalar(c.Name),
			&yaml.Node{Kind: yaml.ScalarNode, Value: "address"}, &yaml.Node{Kind: yaml.ScalarNode, Value: c.Address, Style: yaml.DoubleQuotedStyle},
			&yaml.Node{Kind: yaml.ScalarNode, Value: "abi"}, scalar(c.ABI),
		)

		eventsSeq := &yaml.Node{Kind: yaml.SequenceNode}
		for _, ev := range c.Events {
			evNode := &yaml.Node{Kind: yaml.MappingNode}
			evNode.Content = append(evNode.Content,
				&yaml.Node{Kind: yaml.ScalarNode, Value: "name"}, &yaml.Node{Kind: yaml.ScalarNode, Value: ev.Name, Style: yaml.DoubleQuotedStyle},
			)

			table := &yaml.Node{Kind: yaml.MappingNode}
			table.Content = append(table.Content,
				&yaml.Node{Kind: yaml.ScalarNode, Value: "type"}, scalar(ev.Table.Type),
			)
			if ev.Table.UniqueKey != "" {
				table.Content = append(table.Content,
					&yaml.Node{Kind: yaml.ScalarNode, Value: "unique_key"}, scalar(ev.Table.UniqueKey),
				)
			}
			if len(ev.Table.Aggregates) > 0 {
				aggSeq := &yaml.Node{Kind: yaml.SequenceNode}
				for _, agg := range ev.Table.Aggregates {
					aggNode := &yaml.Node{Kind: yaml.MappingNode}
					aggNode.Content = append(aggNode.Content,
						&yaml.Node{Kind: yaml.ScalarNode, Value: "column"}, scalar(agg.Column),
						&yaml.Node{Kind: yaml.ScalarNode, Value: "operation"}, scalar(agg.Operation),
						&yaml.Node{Kind: yaml.ScalarNode, Value: "field"}, scalar(agg.Field),
					)
					aggSeq.Content = append(aggSeq.Content, aggNode)
				}
				table.Content = append(table.Content,
					&yaml.Node{Kind: yaml.ScalarNode, Value: "aggregate"}, aggSeq,
				)
			}

			evNode.Content = append(evNode.Content,
				&yaml.Node{Kind: yaml.ScalarNode, Value: "table"}, table,
			)
			eventsSeq.Content = append(eventsSeq.Content, evNode)
		}

		cc.Content = append(cc.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Value: "events"}, eventsSeq,
		)
		contractsSeq.Content = append(contractsSeq.Content, cc)
	}
	addKV("contracts", contractsSeq)

	return root
}

// --- Helpers ---

// shortContractName generates a default name from the contract address.
func shortContractName(address string) string {
	if len(address) > 10 {
		return "Contract_" + address[2:8]
	}
	return "Contract"
}

// truncateAddress shortens an address for display.
func truncateAddress(address string) string {
	if len(address) > 14 {
		return address[:8] + "..." + address[len(address)-4:]
	}
	return address
}

// describeEventFields returns a short description of an event's fields.
func describeEventFields(ev *abi.EventDef) string {
	var parts []string
	if len(ev.KeyMembers) > 0 {
		names := make([]string, len(ev.KeyMembers))
		for i, m := range ev.KeyMembers {
			names[i] = m.Name
		}
		parts = append(parts, fmt.Sprintf("keys: %s", strings.Join(names, ", ")))
	}
	if len(ev.DataMembers) > 0 {
		names := make([]string, len(ev.DataMembers))
		for i, m := range ev.DataMembers {
			names[i] = m.Name
		}
		parts = append(parts, fmt.Sprintf("data: %s", strings.Join(names, ", ")))
	}
	if len(parts) == 0 {
		return "no fields"
	}
	return strings.Join(parts, "; ")
}

// allEventFieldNames returns all field names (keys + data) for an event.
func allEventFieldNames(ev *abi.EventDef) []string {
	var names []string
	for _, m := range ev.KeyMembers {
		names = append(names, m.Name)
	}
	for _, m := range ev.DataMembers {
		names = append(names, m.Name)
	}
	return names
}
