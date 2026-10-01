package cli

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// cobra must not print the raw error: main prints it URL-redacted.
func TestRootSilencesErrors(t *testing.T) {
	fail := &cobra.Command{Use: "failtest", RunE: func(*cobra.Command, []string) error {
		return errors.New(`Post "https://node.example/rpc/SECRETTOKEN": EOF`)
	}}
	rootCmd.AddCommand(fail)
	defer rootCmd.RemoveCommand(fail)
	var out bytes.Buffer
	rootCmd.SetOut(&out)
	rootCmd.SetErr(&out)
	rootCmd.SetArgs([]string{"failtest"})
	defer rootCmd.SetArgs(nil)

	if err := Execute(); err == nil {
		t.Fatal("want error")
	}
	if strings.Contains(out.String(), "SECRETTOKEN") {
		t.Errorf("cobra printed the raw error: %s", out.String())
	}
}
