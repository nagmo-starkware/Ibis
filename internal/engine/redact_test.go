package engine

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/b-j-roberts/ibis/internal/provider"
)

// Engine logs built on the root redacting logger never leak the RPC key.
func TestEngineLogMasksRPCURL(t *testing.T) {
	var buf bytes.Buffer
	root := provider.NewRedactingLogger(slog.New(slog.NewTextHandler(&buf, nil)))
	e := New(nil, nil, nil, root)
	e.logger.Error("failed to freeze contract", "error", errors.New(`Post "https://node.example/rpc/SECRETKEY": EOF`))
	out := buf.String()
	if strings.Contains(out, "SECRETKEY") || !strings.Contains(out, "node.example") {
		t.Errorf("not masked: %s", out)
	}
}
