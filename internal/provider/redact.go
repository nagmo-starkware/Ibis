package provider

import (
	"context"
	"log/slog"
	"net/url"
	"regexp"
)

// RPC endpoints carry their API key in the URL path (or query), and Go's HTTP
// and websocket errors embed the full URL ("Post \"https://.../KEY\": ..."), so
// any logged error could leak it. Every provider and subscriber log goes
// through redactHandler, which reduces each URL in a message or attribute to
// scheme://host/<redacted>.

var urlRE = regexp.MustCompile(`(?i)\b(?:https?|wss?)://[^\s"'<>\\]+`)

// redactURLs masks the path, query and userinfo of every URL in s.
func redactURLs(s string) string {
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

func newRedactingLogger(l *slog.Logger) *slog.Logger {
	if _, ok := l.Handler().(redactHandler); ok {
		return l
	}
	return slog.New(redactHandler{l.Handler()})
}

func redactAttr(a slog.Attr) slog.Attr {
	switch a.Value.Kind() {
	case slog.KindString:
		return slog.String(a.Key, redactURLs(a.Value.String()))
	case slog.KindGroup:
		attrs := a.Value.Group()
		out := make([]slog.Attr, len(attrs))
		for i, g := range attrs {
			out[i] = redactAttr(g)
		}
		return slog.Attr{Key: a.Key, Value: slog.GroupValue(out...)}
	case slog.KindAny:
		if err, ok := a.Value.Any().(error); ok {
			return slog.String(a.Key, redactURLs(err.Error()))
		}
	}
	return a
}

func (h redactHandler) Handle(ctx context.Context, r slog.Record) error {
	out := slog.NewRecord(r.Time, r.Level, redactURLs(r.Message), r.PC)
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
