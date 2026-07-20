//go:build !darwin

package monitoring

import (
	"errors"
	"testing"
)

func TestCollectProcessUnsupported(t *testing.T) {
	stats, err := collectProcess(1)
	if !errors.Is(err, ErrProcessStatsUnsupported) || stats != (ProcessStats{}) {
		t.Fatalf("collectProcess() = %#v, %v", stats, err)
	}
}
