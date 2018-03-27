package scheduler

import (
	"testing"

	"code.uber.internal/infra/kraken/utils/memsize"
)

func TestGetBucket(t *testing.T) {
	tests := []struct {
		desc     string
		size     uint64
		expected uint64
	}{
		{"below min", memsize.KB, 10 * memsize.MB},
		{"above max", 30 * memsize.GB, 10 * memsize.GB},
		{"round up", 900 * memsize.MB, memsize.GB},
		{"round down", 400 * memsize.MB, 100 * memsize.MB},
		{"exact bucket", memsize.GB, memsize.GB},
	}
	for _, test := range tests {
		t.Run(test.desc, func(t *testing.T) {
			result := getBucket(test.size)
			if test.expected != result {
				t.Fatalf("Expected %s, got %s", memsize.Format(test.expected), memsize.Format(result))
			}
		})
	}
}