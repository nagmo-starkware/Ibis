package cli

import (
	"bytes"
	"errors"
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

func TestNewRootLoggerRedacts(t *testing.T) {
	var buf bytes.Buffer
	l := newRootLogger(&buf)
	l.Error("boom", "error", errors.New(`Post "https://node.example/rpc/SECRETTOKEN": EOF`))
	l.With("component", "engine").Warn("see https://node.example/rpc/SECRETTOKEN")
	out := buf.String()
	if strings.Contains(out, "SECRETTOKEN") || strings.Count(out, "node.example") != 2 {
		t.Errorf("root logger not redacting: %s", out)
	}
}
