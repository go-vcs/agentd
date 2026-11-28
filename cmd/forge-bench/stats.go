package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
)

// parseLoad reads the three load averages out of the text of uptime(1), in the
// macOS form ("load averages: 7.92 5.95 5.62") and the Linux form ("load
// average: 0.52, 0.58, 0.59"). It never guesses: a text without three numbers
// after the colon is an error, so a failed read cannot pass for a quiet machine.
func parseLoad(s string) (l1, l5, l15 float64, err error) {
	i := strings.Index(s, "load average")
	if i < 0 {
		return 0, 0, 0, errors.New("no load average in the uptime text")
	}
	_, after, _ := strings.Cut(s[i:], ":") // no colon leaves nothing: "want three" below
	f := strings.FieldsFunc(after, func(r rune) bool { return r == ' ' || r == ',' || r == '\t' || r == '\n' })
	if len(f) < 3 {
		return 0, 0, 0, fmt.Errorf("want three load averages, found %d", len(f))
	}
	var v [3]float64
	for k := 0; k < 3; k++ {
		x, err := strconv.ParseFloat(f[k], 64)
		if err != nil || math.IsNaN(x) || x < 0 {
			return 0, 0, 0, fmt.Errorf("load average %q is not a load", f[k])
		}
		v[k] = x
	}
	return v[0], v[1], v[2], nil
}

// gate decides which rounds run: a round whose 1-minute or 5-minute load is
// above max is dropped, and two dropped rounds in a row stop the measurement.
type gate struct {
	max         float64
	consecutive int
}

func (g *gate) admit(l1, l5 float64) (ok, stop bool) {
	if l1 > g.max || l5 > g.max {
		g.consecutive++
		return false, g.consecutive >= 2
	}
	g.consecutive = 0
	return true, false
}

// median of xs (the mean of the two middle values for an even count); NaN for
// no values.
func median(xs []float64) float64 {
	if len(xs) == 0 {
		return math.NaN()
	}
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	n := len(s)
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}

// extent is the smallest and largest of xs.
func extent(xs []float64) (lo, hi float64) {
	if len(xs) == 0 {
		return math.NaN(), math.NaN()
	}
	lo, hi = xs[0], xs[0]
	for _, x := range xs[1:] {
		lo, hi = math.Min(lo, x), math.Max(hi, x)
	}
	return lo, hi
}

// walkTree counts the regular files under dir, and their bytes, leaving out
// the .git directory.
func walkTree(dir string) (files int, bytes int64, err error) {
	return walkFS(os.DirFS(dir))
}

func walkFS(fsys fs.FS) (files int, bytes int64, err error) {
	err = fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && d.Name() == ".git" {
			return fs.SkipDir
		}
		if d.Type().IsRegular() {
			fi, err := d.Info()
			if err != nil {
				return err
			}
			files++
			bytes += fi.Size()
		}
		return nil
	})
	return files, bytes, err
}

// sample is one timed run of one variant.
type sample struct {
	Round     int     `json:"round"`
	Variant   string  `json:"variant"`
	Seconds   float64 `json:"seconds"`
	Requests  int     `json:"requests"`
	Bytes     int64   `json:"bytes"`
	Files     int     `json:"files"`
	FileBytes int64   `json:"file_bytes"`
}

// loadRow is the load read before a round, and after it when it ran.
type loadRow struct {
	Attempt int     `json:"attempt"`
	Round   int     `json:"round"` // 0 when dropped
	Load1   float64 `json:"load1"`
	Load5   float64 `json:"load5"`
	After1  float64 `json:"after1,omitempty"`
	After5  float64 `json:"after5,omitempty"`
	Dropped bool    `json:"dropped"`
}

// row is one variant of the summary.
type row struct {
	Variant          string
	N                int
	Median, Lo, Hi   float64
	MedReq           float64
	MedBytes         float64
	Files            int
	FileBytes        int64
	ConsistentWithin bool // every round produced the same files and bytes
}

// summarize groups the samples by variant, in the order variants first appear.
func summarize(ss []sample) []row {
	var order []string
	by := map[string][]sample{}
	for _, s := range ss {
		if _, ok := by[s.Variant]; !ok {
			order = append(order, s.Variant)
		}
		by[s.Variant] = append(by[s.Variant], s)
	}
	var out []row
	for _, v := range order {
		g := by[v]
		var t, rq, by2 []float64
		same := true
		for _, s := range g {
			t = append(t, s.Seconds)
			rq = append(rq, float64(s.Requests))
			by2 = append(by2, float64(s.Bytes))
			if s.Files != g[0].Files || s.FileBytes != g[0].FileBytes {
				same = false
			}
		}
		lo, hi := extent(t)
		out = append(out, row{Variant: v, N: len(g), Median: median(t), Lo: lo, Hi: hi,
			MedReq: median(rq), MedBytes: median(by2), Files: g[0].Files, FileBytes: g[0].FileBytes, ConsistentWithin: same})
	}
	return out
}

// writeTable renders the summary as a Markdown table, and the load per round.
func writeTable(w io.Writer, rows []row, loads []loadRow) {
	fmt.Fprintln(w, "| variant | rounds | median s | min-max s | upstream requests (median) | upstream bytes (median) | files | file bytes |")
	fmt.Fprintln(w, "|---|---|---|---|---|---|---|---|")
	for _, r := range rows {
		note := ""
		if !r.ConsistentWithin {
			note = " (INCONSISTENT)"
		}
		fmt.Fprintf(w, "| %s | %d | %.2f | %.2f-%.2f | %.0f | %.0f | %d%s | %d |\n",
			r.Variant, r.N, r.Median, r.Lo, r.Hi, r.MedReq, r.MedBytes, r.Files, note, r.FileBytes)
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "| attempt | round | load 1 min / 5 min before | after | |")
	fmt.Fprintln(w, "|---|---|---|---|---|")
	for _, l := range loads {
		if l.Dropped {
			fmt.Fprintf(w, "| %d | - | %.2f / %.2f | - | dropped |\n", l.Attempt, l.Load1, l.Load5)
			continue
		}
		fmt.Fprintf(w, "| %d | %d | %.2f / %.2f | %.2f / %.2f | |\n", l.Attempt, l.Round, l.Load1, l.Load5, l.After1, l.After5)
	}
}

// sparsePatterns turns checkout paths into non-cone sparse-checkout patterns,
// anchored at the repository root.
func sparsePatterns(paths []string) []string {
	out := make([]string, len(paths))
	for i, p := range paths {
		out[i] = "/" + strings.Trim(p, "/")
	}
	return out
}
