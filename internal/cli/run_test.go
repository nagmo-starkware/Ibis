package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/b-j-roberts/ibis/internal/config"
)

func TestPrintConfigSummaryMasksRPC(t *testing.T) {
	var buf bytes.Buffer
	printConfigSummary(&buf, "ibis.yaml", &config.Config{Network: "mainnet", RPC: "https://node.example/rpc/SECRETKEY"})
	out := buf.String()
	if strings.Contains(out, "SECRETKEY") || !strings.Contains(out, "https://node.example") {
		t.Errorf("RPC not masked: %s", out)
	}
}
