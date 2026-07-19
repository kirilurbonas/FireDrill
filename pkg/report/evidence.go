// Package report produces the drill's evidence record: a JSON document with
// measured RTO/RPO, per-check results and sandbox lifecycle, optionally
// signed with ed25519 so auditors can verify it was not tampered with.
package report

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/kirilurbonas/FireDrill/pkg/verify"
)

// Evidence is the audit record for one drill run.
type Evidence struct {
	Drill      string    `json:"drill"`
	Tool       string    `json:"tool"` // firedrill vX.Y.Z
	StartedAt  time.Time `json:"startedAt"`
	FinishedAt time.Time `json:"finishedAt"`

	Backup struct {
		URI     string    `json:"uri"`
		ModTime time.Time `json:"modTime"`
		AgeSecs float64   `json:"ageSeconds"`
		Bytes   int64     `json:"bytes"`
	} `json:"backup"`

	Objectives struct {
		RTO string `json:"rto"`
		RPO string `json:"rpo"`
	} `json:"objectives"`

	Measured struct {
		RestoreSeconds float64 `json:"restoreSeconds"` // measured RTO
		RTOMet         bool    `json:"rtoMet"`
		RPOMet         bool    `json:"rpoMet"`
	} `json:"measured"`

	Sandbox struct {
		Provider  string `json:"provider"`
		Image     string `json:"image"`
		Destroyed bool   `json:"destroyed"`
	} `json:"sandbox"`

	Checks   []verify.Result `json:"checks"`
	Controls []string        `json:"controls,omitempty"`
	Verified bool            `json:"verified"` // overall pass/fail
	Error    string          `json:"error,omitempty"`
}

// Write stores the evidence as canonical JSON in dir and returns its path.
func (e *Evidence) Write(dir string) (string, error) {
	if dir == "" {
		dir = "evidence"
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", err
	}
	// Nanosecond suffix: two runs of the same drill within one second must
	// never overwrite each other's audit records.
	name := fmt.Sprintf("%s-%s-%09d.json", e.Drill,
		e.FinishedAt.UTC().Format("2006-01-02T15-04-05Z"), e.FinishedAt.Nanosecond())
	path := filepath.Join(dir, name)
	data, err := Canonical(e)
	if err != nil {
		return "", err
	}
	// Atomic write (temp + rename): a crash mid-write must never leave a
	// truncated evidence file that then gets signed.
	tmp := path + ".tmp"
	// #nosec G306 -- evidence is meant to be shared with auditors, not secret
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, path); err != nil {
		return "", err
	}
	return path, nil
}

// Canonical renders evidence deterministically (stable field order via the
// struct definition, no trailing whitespace) so signatures are reproducible.
func Canonical(e *Evidence) ([]byte, error) {
	return json.MarshalIndent(e, "", "  ")
}
