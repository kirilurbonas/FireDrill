package spec

import (
	"embed"
	"fmt"
	"sort"
	"strings"
)

//go:embed firedrill.schema.json
var schemaJSON []byte

//go:embed templates/*.yaml
var templates embed.FS

// Schema returns the JSON Schema for a drill document. Write it next to a
// spec and editors (VS Code, Neovim, anything speaking yaml-language-server)
// autocomplete and validate the file as it is typed.
func Schema() []byte { return schemaJSON }

// Template returns a commented starter spec for a driver.
func Template(driver string) ([]byte, error) {
	data, err := templates.ReadFile("templates/" + driver + ".yaml")
	if err != nil {
		return nil, fmt.Errorf("no template for driver %q (available: %s)", driver, strings.Join(TemplateDrivers(), ", "))
	}
	return data, nil
}

// TemplateDrivers lists the drivers `firedrill init` can scaffold.
func TemplateDrivers() []string {
	entries, err := templates.ReadDir("templates")
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, strings.TrimSuffix(e.Name(), ".yaml"))
	}
	sort.Strings(out)
	return out
}
