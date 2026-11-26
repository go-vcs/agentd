package ghapi

import (
	"os"
	"path/filepath"
	"testing"
)

func seed(f *testing.F) {
	m, _ := filepath.Glob(filepath.Join("testdata", "*.json"))
	for _, p := range m {
		b, err := os.ReadFile(p)
		if err != nil {
			f.Fatal(err)
		}
		f.Add(b)
	}
	f.Add([]byte(`{"total_count":-1,"workflow_runs":[null]}`))
	f.Add([]byte(`[`))
}

// A decoder of upstream bytes must never panic, and a job count it returns is
// a real, non-negative measurement.
func FuzzDecodeRuns(f *testing.F) {
	seed(f)
	f.Fuzz(func(t *testing.T, b []byte) {
		runs, _, err := DecodeRuns(b)
		if err == nil {
			for _, r := range runs {
				if r.Jobs != -1 {
					t.Fatalf("a decoded run claims %d jobs before any were counted", r.Jobs)
				}
			}
		}
	})
}

func FuzzDecodeChecks(f *testing.F) {
	seed(f)
	f.Fuzz(func(t *testing.T, b []byte) { _, _, _ = DecodeChecks(b) })
}

func FuzzDecodeJobCount(f *testing.F) {
	seed(f)
	f.Fuzz(func(t *testing.T, b []byte) {
		defer noCrash(t)
		if n, err := DecodeJobCount(b); err == nil && n < 0 {
			t.Fatalf("negative job count %d", n)
		}
	})
}
