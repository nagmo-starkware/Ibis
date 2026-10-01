package provider

import (
	"bytes"
	"errors"
	"log/slog"
	"net/url"
	"strings"
	"testing"
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
