package provider

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"regexp"
	"strings"
)

// RPC endpoints carry their API key in the URL path (or query), and Go's HTTP
// and websocket errors embed the full URL ("Post \"https://.../KEY\": ..."), so
// any logged error could leak it. Every provider and subscriber log goes
// through redactHandler, which reduces each URL in a message or attribute to
// scheme://host/<redacted>.

var urlRE = regexp.MustCompile(`(?i)\b(?:https?|wss?)://[^\s"'<>\\]+`)

// RedactURLs masks the path, query and userinfo of every URL in s.
func RedactURLs(s string) string {
	if !strings.Contains(s, "://") {
		return s
	}
	return urlRE.ReplaceAllStringFunc(s, func(m string) string {
		u, err := url.Parse(m)
		if err != nil || u.Host == "" {
			return "<redacted-url>"
		}
		out := u.Scheme + "://" + u.Host
		if u.Path != "" && u.Path != "/" || u.RawQuery != "" {
			out += "/<redacted>"
		}
		return out
	})
}

type redactHandler struct{ slog.Handler }

// NewRedactingLogger wraps l so every URL it logs is masked; idempotent.
func NewRedactingLogger(l *slog.Logger) *slog.Logger {
	if _, ok := l.Handler().(redactHandler); ok {
		return l
	}
	return slog.New(redactHandler{l.Handler()})
}

func redactAttr(a slog.Attr) slog.Attr {
	switch a.Value.Kind() {
	case slog.KindString:
		return slog.String(a.Key, RedactURLs(a.Value.String()))
	case slog.KindGroup:
		attrs := a.Value.Group()
		out := make([]slog.Attr, len(attrs))
		for i, g := range attrs {
			out[i] = redactAttr(g)
		}
		return slog.Attr{Key: a.Key, Value: slog.GroupValue(out...)}
	case slog.KindLogValuer:
		a.Value = a.Value.Resolve()
		return redactAttr(a)
	case slog.KindAny:
		switch v := a.Value.Any().(type) {
		case error:
			if str, ok := safeString(v.Error); ok {
				return slog.String(a.Key, RedactURLs(str))
			}
		case fmt.Stringer:
			if str, ok := safeString(v.String); ok {
				if red := RedactURLs(str); red != str {
					return slog.String(a.Key, red)
				}
			}
		}
	}
	return a
}

func (h redactHandler) Handle(ctx context.Context, r slog.Record) error {
	out := slog.NewRecord(r.Time, r.Level, RedactURLs(r.Message), r.PC)
	r.Attrs(func(a slog.Attr) bool { out.AddAttrs(redactAttr(a)); return true })
	return h.Handler.Handle(ctx, out)
}

func (h redactHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	red := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		red[i] = redactAttr(a)
	}
	return redactHandler{h.Handler.WithAttrs(red)}
}

func (h redactHandler) WithGroup(name string) slog.Handler {
	return redactHandler{h.Handler.WithGroup(name)}
}

// safeString calls f, reporting false if it panics (typed-nil receiver).
func safeString(f func() string) (s string, ok bool) {
	defer func() {
		if recover() != nil {
			s, ok = "", false
		}
	}()
	return f(), true
}
