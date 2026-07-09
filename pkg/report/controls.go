package report

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// ControlEntry is one drill run attributed to a compliance control.
type ControlEntry struct {
	Drill        string    `json:"drill"`
	FinishedAt   time.Time `json:"finishedAt"`
	Verified     bool      `json:"verified"`
	RestoreSecs  float64   `json:"restoreSeconds"`
	RTOMet       bool      `json:"rtoMet"`
	RPOMet       bool      `json:"rpoMet"`
	EvidenceFile string    `json:"evidenceFile"`
	Signed       bool      `json:"signed"` // signature file present and valid
}

// ControlReport maps each compliance control to the drill runs that
// evidence it — the artifact a GRC team hands to an auditor.
type ControlReport struct {
	GeneratedAt time.Time                 `json:"generatedAt"`
	EvidenceDir string                    `json:"evidenceDir"`
	Controls    map[string][]ControlEntry `json:"controls"`
	// Unattributed counts evidence files that declare no controls.
	Unattributed int `json:"unattributed"`
}

// BuildControlReport scans dir for evidence JSON files and groups them by
// declared control. Files whose detached signature exists and validates are
// marked Signed; files that fail signature validation are still listed
// (auditors should see them) but Signed=false.
func BuildControlReport(dir string) (*ControlReport, error) {
	matches, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, err
	}
	rep := &ControlReport{
		GeneratedAt: time.Now().UTC(),
		EvidenceDir: dir,
		Controls:    map[string][]ControlEntry{},
	}
	for _, path := range matches {
		data, err := os.ReadFile(path) // #nosec G304 -- user-designated evidence dir
		if err != nil {
			return nil, err
		}
		var e Evidence
		if err := json.Unmarshal(data, &e); err != nil {
			continue // not an evidence file
		}
		if e.Drill == "" {
			continue
		}
		entry := ControlEntry{
			Drill:        e.Drill,
			FinishedAt:   e.FinishedAt,
			Verified:     e.Verified,
			RestoreSecs:  e.Measured.RestoreSeconds,
			RTOMet:       e.Measured.RTOMet,
			RPOMet:       e.Measured.RPOMet,
			EvidenceFile: filepath.Base(path),
			Signed:       Verify(path, nil) == nil,
		}
		if len(e.Controls) == 0 {
			rep.Unattributed++
			continue
		}
		for _, c := range e.Controls {
			rep.Controls[c] = append(rep.Controls[c], entry)
		}
	}
	for _, entries := range rep.Controls {
		sort.Slice(entries, func(i, j int) bool { return entries[i].FinishedAt.After(entries[j].FinishedAt) })
	}
	return rep, nil
}

// WriteMarkdown renders the control report as an auditor-readable document.
func (r *ControlReport) WriteMarkdown(w io.Writer) error {
	fmt.Fprintf(w, "# Recovery-testing evidence by control\n\n")
	fmt.Fprintf(w, "Generated %s from `%s`.\n\n", r.GeneratedAt.Format("2006-01-02 15:04 UTC"), r.EvidenceDir)
	if len(r.Controls) == 0 {
		fmt.Fprintln(w, "_No evidence with declared controls found._")
		return nil
	}
	controls := make([]string, 0, len(r.Controls))
	for c := range r.Controls {
		controls = append(controls, c)
	}
	sort.Strings(controls)

	for _, c := range controls {
		entries := r.Controls[c]
		passed := 0
		for _, e := range entries {
			if e.Verified {
				passed++
			}
		}
		fmt.Fprintf(w, "## %s\n\n", c)
		fmt.Fprintf(w, "%d drill run(s), %d verified. Latest run %s.\n\n",
			len(entries), passed, entries[0].FinishedAt.Format("2006-01-02"))
		fmt.Fprintln(w, "| Date (UTC) | Drill | Result | Restore time | RTO | RPO | Signed | Evidence |")
		fmt.Fprintln(w, "|---|---|---|---|---|---|---|---|")
		for _, e := range entries {
			result := "❌ failed"
			if e.Verified {
				result = "✅ verified"
			}
			fmt.Fprintf(w, "| %s | %s | %s | %s | %s | %s | %s | `%s` |\n",
				e.FinishedAt.Format("2006-01-02 15:04"), e.Drill, result,
				(time.Duration(e.RestoreSecs * float64(time.Second))).Round(time.Second),
				met(e.RTOMet), met(e.RPOMet), yes(e.Signed), e.EvidenceFile)
		}
		fmt.Fprintln(w)
	}
	if r.Unattributed > 0 {
		fmt.Fprintf(w, "_%d evidence file(s) declare no controls and are not attributed._\n", r.Unattributed)
	}
	return nil
}

func met(ok bool) string {
	if ok {
		return "met"
	}
	return "missed"
}

func yes(ok bool) string {
	if ok {
		return "✓"
	}
	return "—"
}

// WriteJSON renders the machine-readable control report.
func (r *ControlReport) WriteJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}
