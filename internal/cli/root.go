package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

var cfgPath string

// SetVersion configures the version output for --version.
func SetVersion(version, commit, date string) {
	rootCmd.Version = fmt.Sprintf("%s (commit: %s, built: %s)", version, commit, date)
}

var rootCmd = &cobra.Command{
	Use:   "ibis",
	Short: "A fast, easy-to-use Starknet event indexer",
	Long: `Ibis indexes events from Starknet smart contracts using only an RPC
connection, generates typed database tables and REST APIs from contract
ABIs, and launches with a single command from a YAML config file.`,
}

func Execute() error {
	return rootCmd.Execute()
}

func init() {
	rootCmd.PersistentFlags().StringVar(&cfgPath, "config", "./ibis.config.yaml", "path to ibis config file")

	rootCmd.AddCommand(initCmd)
	rootCmd.AddCommand(runCmd)
	rootCmd.AddCommand(queryCmd)
	rootCmd.AddCommand(serveCmd)
}
