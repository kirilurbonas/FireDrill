package report

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// HistoryEntry is one past drill run, summarized for trend display.
type HistoryEntry struct {
	Drill       string
	FinishedAt  time.Time
	Verified    bool
	RestoreSecs float64
	BackupAge   float64
	RTOMet      bool
	RPOMet      bool
	File        string
}

// LoadHistory reads every evidence file in dir, optionally filtered by
// drill name, sorted oldest → newest.
func LoadHistory(dir, drill string) ([]HistoryEntry, error) {
	matches, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, err
	}
	var out []HistoryEntry
	for _, path := range matches {
		data, err := os.ReadFile(path) // #nosec G304 -- user-designated evidence dir
		if err != nil {
			return nil, err
		}
		var e Evidence
		if err := json.Unmarshal(data, &e); err != nil || e.Drill == "" {
			continue // not an evidence file
		}
		if drill != "" && e.Drill != drill {
			continue
		}
		out = append(out, HistoryEntry{
			Drill:       e.Drill,
			FinishedAt:  e.FinishedAt,
			Verified:    e.Verified,
			RestoreSecs: e.Measured.RestoreSeconds,
			BackupAge:   e.Backup.AgeSecs,
			RTOMet:      e.Measured.RTOMet,
			RPOMet:      e.Measured.RPOMet,
			File:        filepath.Base(path),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].FinishedAt.Before(out[j].FinishedAt) })
	return out, nil
}

// WriteHistory renders the drill history as a terminal table with a
// spark-style RTO trend so regressions are visible at a glance.
func WriteHistory(w io.Writer, entries []HistoryEntry) {
	if len(entries) == 0 {
		fmt.Fprintln(w, "no drill evidence found")
		return
	}
	maxRTO := 0.0
	for _, e := range entries {
		if e.RestoreSecs > maxRTO {
			maxRTO = e.RestoreSecs
		}
	}

	fmt.Fprintf(w, "%-19s  %-16s  %-8s  %-10s  %-4s %-4s  %s\n",
		"WHEN (UTC)", "DRILL", "RESULT", "RESTORE", "RTO", "RPO", "TREND")
	for _, e := range entries {
		result := "FAILED"
		if e.Verified {
			result = "ok"
		}
		fmt.Fprintf(w, "%-19s  %-16s  %-8s  %-10s  %-4s %-4s  %s\n",
			e.FinishedAt.UTC().Format("2006-01-02 15:04"),
			trunc(e.Drill, 16),
			result,
			(time.Duration(e.RestoreSecs * float64(time.Second))).Round(time.Second).String(),
			mark(e.RTOMet), mark(e.RPOMet),
			bar(e.RestoreSecs, maxRTO, 20))
	}

	// Verification rate summary.
	pass := 0
	for _, e := range entries {
		if e.Verified {
			pass++
		}
	}
	fmt.Fprintf(w, "\n%d run(s), %d verified (%.0f%%)\n", len(entries), pass,
		100*float64(pass)/float64(len(entries)))
}

func mark(ok bool) string {
	if ok {
		return "✓"
	}
	return "✗"
}

func bar(v, max float64, width int) string {
	if max <= 0 {
		return ""
	}
	n := int(v / max * float64(width))
	if n < 1 && v > 0 {
		n = 1
	}
	return strings.Repeat("▇", n)
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
