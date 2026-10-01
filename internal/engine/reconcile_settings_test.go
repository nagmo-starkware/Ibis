package engine

import (
	"testing"
	"time"

	"github.com/b-j-roberts/ibis/internal/config"
	"github.com/b-j-roberts/ibis/internal/provider"
)

func TestReconcileSettings(t *testing.T) {
	zero, four, neg := 0, 4, -1
	for _, tc := range []struct {
		name         string
		ic           config.IndexerConfig
		wantInterval time.Duration
		wantLag      *uint64
	}{
		{"omitted means on at the default", config.IndexerConfig{}, provider.DefaultReconcileInterval, nil},
		{"explicit 0s is off", config.IndexerConfig{ReconcileInterval: "0s"}, 0, nil},
		{"explicit interval", config.IndexerConfig{ReconcileInterval: "30s"}, 30 * time.Second, nil},
		{"lag unset is the provider default", config.IndexerConfig{ReconcileInterval: "5s"}, 5 * time.Second, nil},
		{"lag 0 is explicit", config.IndexerConfig{ReconcileLag: &zero}, provider.DefaultReconcileInterval, ptr(0)},
		{"lag 4", config.IndexerConfig{ReconcileLag: &four}, provider.DefaultReconcileInterval, ptr(4)},
		{"negative lag is ignored (rejected by validation)", config.IndexerConfig{ReconcileLag: &neg}, provider.DefaultReconcileInterval, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ic := tc.ic
			interval, lag := reconcileSettings(&ic)
			if interval != tc.wantInterval {
				t.Errorf("interval = %v, want %v", interval, tc.wantInterval)
			}
			switch {
			case lag == nil && tc.wantLag != nil, lag != nil && tc.wantLag == nil, lag != nil && *lag != *tc.wantLag:
				t.Errorf("lag = %v, want %v", lag, tc.wantLag)
			}
		})
	}
}

func ptr(v uint64) *uint64 { return &v }
