package report

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// evidenceFile pairs an evidence record with the file it was read from.
type evidenceFile struct {
	Path string
	E    Evidence
}

// scanEvidence reads every evidence JSON in dir. Files that are not evidence
// records are skipped, not errors: the directory is also home to signatures,
// attestations and HTML reports.
func scanEvidence(dir string) ([]evidenceFile, error) {
	matches, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, err
	}
	out := make([]evidenceFile, 0, len(matches))
	for _, path := range matches {
		data, err := os.ReadFile(path) // #nosec G304 -- user-designated evidence dir
		if err != nil {
			return nil, err
		}
		var e Evidence
		if err := json.Unmarshal(data, &e); err != nil || e.Drill == "" {
			continue
		}
		out = append(out, evidenceFile{Path: path, E: e})
	}
	return out, nil
}
