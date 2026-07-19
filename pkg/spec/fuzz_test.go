package spec

import (
	"strings"
	"testing"
)

// FuzzParseAll hardens the spec parser against malformed input: it must
// return an error or a valid drill list — never panic or hang.
func FuzzParseAll(f *testing.F) {
	f.Add(valid)
	f.Add(validVelero)
	f.Add(valid + "\n---\n" + validVelero)
	f.Add("")
	f.Add("apiVersion: firedrill.dev/v1")
	f.Add("kind: [1,2,3]\nmetadata: 7")
	f.Add(strings.Repeat("---\n", 50))
	f.Fuzz(func(t *testing.T, doc string) {
		drills, err := ParseAll(strings.NewReader(doc))
		if err == nil {
			for _, d := range drills {
				if d.Metadata.Name == "" {
					t.Fatal("parsed drill with empty name and no error")
				}
			}
		}
	})
}
