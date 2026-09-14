package provider

import (
	"errors"
	"fmt"
	"testing"
)

func TestRedactURL(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "api key as path segment",
			input: `Post "https://starknet-mainnet.g.alchemy.com/starknet/version/rpc/v0_10/super-secret-key": context deadline exceeded`,
			want:  `Post "https://starknet-mainnet.g.alchemy.com/<redacted>": context deadline exceeded`,
		},
		{
			name:  "api key in query string",
			input: `dial failed: https://rpc.example.com/v1?apiKey=super-secret-key&other=1 unreachable`,
			want:  `dial failed: https://rpc.example.com/<redacted> unreachable`,
		},
		{
			name:  "userinfo credentials",
			input: `connecting to https://user:pass@rpc.example.com/v1/key failed`,
			want:  `connecting to https://rpc.example.com/<redacted> failed`,
		},
		{
			name:  "ws scheme",
			input: `dial ws://rpc.example.com/v1/key: connection refused`,
			want:  `dial ws://rpc.example.com/<redacted>: connection refused`,
		},
		{
			name:  "wss scheme",
			input: `subscribing to wss://starknet-mainnet.g.alchemy.com/starknet/version/rpc/v0_10/super-secret-key: EOF`,
			want:  `subscribing to wss://starknet-mainnet.g.alchemy.com/<redacted>: EOF`,
		},
		{
			name:  "no URL present",
			input: `context deadline exceeded`,
			want:  `context deadline exceeded`,
		},
		{
			name:  "multiple URLs in one message",
			input: `retrying https://a.example.com/key1 after https://b.example.com/key2 failed`,
			want:  `retrying https://a.example.com/<redacted> after https://b.example.com/<redacted> failed`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := RedactURL(tt.input)
			if got != tt.want {
				t.Errorf("RedactURL(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestRedactErr(t *testing.T) {
	t.Run("nil error returns nil", func(t *testing.T) {
		if got := RedactErr(nil); got != nil {
			t.Errorf("RedactErr(nil) = %v, want nil", got)
		}
	})

	t.Run("redacts message and preserves wrapping via errors.Is", func(t *testing.T) {
		sentinel := errors.New("deadline exceeded")
		wrapped := fmt.Errorf(`fetching events: -32603 Post "https://rpc.example.com/v1/super-secret-key": %w`, sentinel)

		redacted := RedactErr(wrapped)

		wantMsg := `fetching events: -32603 Post "https://rpc.example.com/<redacted>": deadline exceeded`
		if redacted.Error() != wantMsg {
			t.Errorf("redacted.Error() = %q, want %q", redacted.Error(), wantMsg)
		}
		if !errors.Is(redacted, sentinel) {
			t.Errorf("errors.Is(redacted, sentinel) = false, want true")
		}

		// Wrapping the redacted error again with %w must keep the chain intact
		// and must not reintroduce the raw URL.
		outer := fmt.Errorf("catchup: %w", redacted)
		if !errors.Is(outer, sentinel) {
			t.Errorf("errors.Is(outer, sentinel) = false, want true")
		}
		if got, want := outer.Error(), "catchup: "+wantMsg; got != want {
			t.Errorf("outer.Error() = %q, want %q", got, want)
		}
	})

	t.Run("no URL leaves error untouched", func(t *testing.T) {
		err := errors.New("context deadline exceeded")
		if got := RedactErr(err); got != err {
			t.Errorf("RedactErr returned a different error for a message with no URL: %v", got)
		}
	})
}
