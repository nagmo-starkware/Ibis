package provider

import (
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
