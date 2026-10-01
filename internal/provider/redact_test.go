package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/url"
	"strings"
	"testing"

	"github.com/NethermindEth/juno/core/felt"
	"github.com/NethermindEth/starknet.go/rpc"
)

const secretURL = "https://node.example/rpc/v0_10/SECRETKEY"

type valuer struct{}

func (valuer) LogValue() slog.Value { return slog.StringValue("via " + secretURL) }

type stringer struct{}

func (stringer) String() string { return secretURL }

func redactedLog(t *testing.T, f func(*slog.Logger)) string {
	t.Helper()
	var buf bytes.Buffer
	f(NewRedactingLogger(slog.New(slog.NewTextHandler(&buf, nil))))
	out := buf.String()
	if strings.Contains(out, "SECRETKEY") || strings.Contains(out, "v0_10") {
		t.Fatalf("secret leaked: %s", out)
	}
	if !strings.Contains(out, "node.example") {
		t.Fatalf("host missing: %s", out)
	}
	return out
}

func TestRedactingLoggerPaths(t *testing.T) {
	u, _ := url.Parse(secretURL)
	cases := map[string]func(*slog.Logger){
		"WithAttrs":  func(l *slog.Logger) { l.With("u", secretURL).Info("m") },
		"WithGroup":  func(l *slog.Logger) { l.WithGroup("g").Info("m", "u", secretURL) },
		"error":      func(l *slog.Logger) { l.Error("m", "error", errors.New("Post \""+secretURL+"\": EOF")) },
		"Stringer":   func(l *slog.Logger) { l.Info("m", "u", stringer{}) },
		"url.URL":    func(l *slog.Logger) { l.Info("m", "u", u) },
		"LogValuer":  func(l *slog.Logger) { l.Info("m", "u", valuer{}) },
		"WithAttrs+": func(l *slog.Logger) { l.With("e", stringer{}).Info("m") },
	}
	for name, f := range cases {
		t.Run(name, func(t *testing.T) { redactedLog(t, f) })
	}
}

func TestNewRedactingLoggerIdempotent(t *testing.T) {
	l := NewRedactingLogger(slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	if NewRedactingLogger(l) != l {
		t.Error("wrapped twice")
	}
}

func TestRedactedErrorKeepsChain(t *testing.T) {
	base := errors.New("Post \"" + secretURL + "\": EOF")
	err := error(redactedError{base})
	if strings.Contains(err.Error(), "SECRETKEY") || !errors.Is(err, base) {
		t.Errorf("bad redactedError: %v", err)
	}
}

type nilStringer struct{ s *string }

func (n *nilStringer) String() string { return *n.s }

type errValuer struct{}

func (errValuer) LogValue() slog.Value { return slog.AnyValue(errors.New("x " + secretURL)) }

type groupValuer struct{}

func (groupValuer) LogValue() slog.Value {
	return slog.GroupValue(slog.String("u", secretURL), slog.Group("in", slog.Any("s", stringer{})))
}

func TestRedactingLoggerEdges(t *testing.T) {
	cases := map[string]func(*slog.Logger){
		"valuer->error": func(l *slog.Logger) { l.Info("m", "v", errValuer{}) },
		"valuer->group": func(l *slog.Logger) { l.Info("m", "v", groupValuer{}) },
		"nested groups": func(l *slog.Logger) {
			l.Info("m", slog.Group("a", slog.Group("b", slog.String("u", secretURL))))
		},
	}
	for name, f := range cases {
		t.Run(name, func(t *testing.T) { redactedLog(t, f) })
	}
	t.Run("typed-nil Stringer does not panic", func(t *testing.T) {
		var buf bytes.Buffer
		l := NewRedactingLogger(slog.New(slog.NewTextHandler(&buf, nil)))
		var ns *nilStringer
		l.Info("m", "v", ns) // must not panic
		l.Info("m", "v", &nilStringer{})
	})
}

// Real failing RPC calls (closed server, secret in the URL path) never leak the token.
func TestProviderErrorsMaskRPCURL(t *testing.T) {
	srv := mockRPCServer(t, map[string]func(json.RawMessage) (interface{}, error){})
	prov, err := New(context.Background(), srv.URL+"/rpc/SECRETTOKEN", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	srv.Close()
	ctx := context.Background()
	f := new(felt.Felt)
	calls := map[string]func() error{
		"BlockNumber":             func() error { _, e := prov.BlockNumber(ctx); return e },
		"CachedBlockNumber":       func() error { _, e := prov.CachedBlockNumber(ctx); return e },
		"PreConfirmedBlockNumber": func() error { _, e := prov.PreConfirmedBlockNumber(ctx); return e },
		"GetBlockTimestamp":       func() error { _, e := prov.GetBlockTimestamp(ctx, 1); return e },
		"GetEvents":               func() error { _, e := prov.GetEvents(ctx, GetEventsOptions{FromBlock: 1, ToBlock: 2}); return e },
		"Call":                    func() error { _, e := prov.Call(ctx, f, f, nil, rpc.BlockID{Tag: rpc.BlockTagLatest}); return e },
		"ClassAt":                 func() error { _, e := prov.ClassAt(ctx, rpc.BlockID{Tag: rpc.BlockTagLatest}, f); return e },
		"GetClassAt":              func() error { _, e := prov.GetClassAt(ctx, f); return e },
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			e := call()
			if e == nil {
				t.Fatal("expected error from closed server")
			}
			if strings.Contains(e.Error(), "SECRETTOKEN") {
				t.Errorf("token leaked: %v", e)
			}
			var re redactedError
			if !errors.As(e, &re) {
				t.Errorf("not a redactedError chain: %T", e)
			}
		})
	}
}

type nilErr struct{ msg string }

func (e *nilErr) Error() string { return e.msg }

// A typed-nil error attr must not panic the handler.
func TestRedactAttrTypedNilError(t *testing.T) {
	var e *nilErr
	var buf bytes.Buffer
	l := NewRedactingLogger(slog.New(slog.NewTextHandler(&buf, nil)))
	l.Error("boom", "error", error(e))
	if !strings.Contains(buf.String(), "boom") {
		t.Errorf("not logged: %s", buf.String())
	}
}
