package report

import (
	_ "embed"
	"html/template"
	"os"
	"strings"
	"time"
)

//go:embed report.html.tmpl
var htmlTemplate string

var reportTmpl = template.Must(template.New("report").Funcs(template.FuncMap{
	"secs": func(s float64) string {
		return (time.Duration(s * float64(time.Second))).Round(time.Second).String()
	},
	"utc": func(t time.Time) string { return t.UTC().Format("2006-01-02 15:04:05 UTC") },
}).Parse(htmlTemplate))

// WriteHTML renders the evidence as a self-contained HTML report next to the
// JSON evidence file (same name, .html extension) and returns its path.
func WriteHTML(e *Evidence, evidencePath string) (string, error) {
	path := strings.TrimSuffix(evidencePath, ".json") + ".html"
	f, err := os.Create(path) // #nosec G304 -- derived from the evidence path we just wrote
	if err != nil {
		return "", err
	}
	if err := reportTmpl.Execute(f, e); err != nil {
		_ = f.Close()
		return "", err
	}
	return path, f.Close()
}
