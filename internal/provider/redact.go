package provider

import (
	"net/url"
	"regexp"
	"strings"
)

// urlPattern matches http(s)/ws(s) URLs embedded in free-form text (error
// messages, log lines). It stops at whitespace or common quoting/delimiter
// characters so a URL embedded in `Post "https://...": timeout` or similar
// wrapped text is matched without swallowing the surrounding message.
var urlPattern = regexp.MustCompile(`(?i)\b(?:https?|wss?)://[^\s"'` + "`" + `<>,)]+`)

// RedactURL returns s with every embedded http, https, ws, or wss URL reduced
// to "scheme://host/<redacted>". This drops the path, query string, and any
// userinfo (user:pass@host), since RPC provider API keys show up in all three
// depending on the provider (e.g. Alchemy puts the key in the path, others use
// a query param or basic auth). Text with no embedded URL is returned
// unchanged.
func RedactURL(s string) string {
	return urlPattern.ReplaceAllStringFunc(s, redactOneURL)
}

// trailingPunct holds characters that terminate a sentence or an error's
// "url: message" separator rather than a URL itself (e.g. the ':' in `dial
// ws://host/key: connection refused`). They're trimmed off the match before
// parsing and reattached after redaction, so surrounding punctuation survives.
const trailingPunct = ":;,.!?"

func redactOneURL(raw string) string {
	trimmed := strings.TrimRight(raw, trailingPunct)
	suffix := raw[len(trimmed):]

	u, err := url.Parse(trimmed)
	if err != nil || u.Host == "" {
		// Not a URL we can safely parse — leave it alone rather than risk
		// mangling unrelated text.
		return raw
	}
	return strings.ToLower(u.Scheme) + "://" + u.Host + "/<redacted>" + suffix
}

// redactedError wraps an error to present a redacted message via Error()
// while keeping the original error reachable via Unwrap, so errors.Is/As
// still see through to it.
type redactedError struct {
	msg string
	err error
}

func (r *redactedError) Error() string { return r.msg }
func (r *redactedError) Unwrap() error { return r.err }

// RedactErr returns err with any embedded URL in its message redacted (see
// RedactURL). It preserves the original error for errors.Is/As via Unwrap, so
// wrapping it with fmt.Errorf("...: %w", RedactErr(err)) keeps normal error
// chain behavior. Returns nil for a nil err, and returns err unchanged if its
// message has nothing to redact.
func RedactErr(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	redacted := RedactURL(msg)
	if redacted == msg {
		return err
	}
	return &redactedError{msg: redacted, err: err}
}
