package spec

import (
	"bytes"
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func TestTemplatesAreValidDrills(t *testing.T) {
	drivers := TemplateDrivers()
	if len(drivers) == 0 {
		t.Fatal("no templates embedded")
	}
	for _, driver := range drivers {
		data, err := Template(driver)
		if err != nil {
			t.Fatalf("%s: %v", driver, err)
		}
		// A starter spec that does not validate is worse than none at all.
		drills, err := ParseAll(bytes.NewReader(data))
		if err != nil {
			t.Fatalf("%s template does not validate: %v", driver, err)
		}
		if got := drills[0].Spec.Source.Driver; got != driver {
			t.Errorf("%s template declares driver %q", driver, got)
		}
		if !bytes.Contains(data, []byte("yaml-language-server: $schema=")) {
			t.Errorf("%s template has no editor schema modeline", driver)
		}
	}
	if _, err := Template("cassandra"); err == nil {
		t.Error("expected an error for an unknown driver")
	}
}

// TestSchemaMatchesSpec keeps docs and code from drifting: every YAML field
// the parser accepts must be described by the schema, and the schema must
// not promise fields the parser would reject (it decodes with KnownFields).
func TestSchemaMatchesSpec(t *testing.T) {
	var doc map[string]any
	if err := json.Unmarshal(Schema(), &doc); err != nil {
		t.Fatalf("schema is not valid JSON: %v", err)
	}

	inSchema := map[string]bool{}
	collectProperties(doc, inSchema)
	inSpec := map[string]bool{}
	collectYAMLFields(reflect.TypeOf(Drill{}), inSpec, map[reflect.Type]bool{})

	var missing, extra []string
	for f := range inSpec {
		if !inSchema[f] {
			missing = append(missing, f)
		}
	}
	for f := range inSchema {
		if !inSpec[f] {
			extra = append(extra, f)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	if len(missing) > 0 {
		t.Errorf("spec fields absent from firedrill.schema.json: %s", strings.Join(missing, ", "))
	}
	if len(extra) > 0 {
		t.Errorf("schema describes fields the parser would reject: %s", strings.Join(extra, ", "))
	}
}

// collectProperties gathers every key under every "properties" object.
func collectProperties(node any, out map[string]bool) {
	switch n := node.(type) {
	case map[string]any:
		for k, v := range n {
			if k == "properties" {
				if props, ok := v.(map[string]any); ok {
					for name := range props {
						out[name] = true
					}
				}
			}
			collectProperties(v, out)
		}
	case []any:
		for _, v := range n {
			collectProperties(v, out)
		}
	}
}

// collectYAMLFields gathers every yaml tag name reachable from t.
func collectYAMLFields(t reflect.Type, out map[string]bool, seen map[reflect.Type]bool) {
	for t.Kind() == reflect.Pointer || t.Kind() == reflect.Slice {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct || seen[t] {
		return
	}
	seen[t] = true
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		tag := strings.Split(f.Tag.Get("yaml"), ",")[0]
		if tag != "" && tag != "-" {
			out[tag] = true
		}
		collectYAMLFields(f.Type, out, seen)
	}
}
