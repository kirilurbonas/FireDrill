package mongodb

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/kirilurbonas/FireDrill/pkg/spec"
)

// fakeSandbox records the command it was asked to run and replays a scripted
// result, so the engine's script assembly and output parsing are testable
// without Docker.
type fakeSandbox struct {
	cmds [][]string
	out  string
	code int
	err  error
}

func (f *fakeSandbox) Exec(_ context.Context, cmd []string, _ io.Reader) (int, string, error) {
	f.cmds = append(f.cmds, cmd)
	return f.code, f.out, f.err
}
func (f *fakeSandbox) Host() string     { return "127.0.0.1" }
func (f *fakeSandbox) HostPort() string { return "27017" }
func (f *fakeSandbox) User() string     { return "firedrill" }
func (f *fakeSandbox) Password() string { return "s3cr3t" }
func (f *fakeSandbox) DB() string       { return "firedrill" }

func TestEngineParsesResults(t *testing.T) {
	ctx := context.Background()

	// mongosh often prints a notice before the value; the last line is the result.
	sb := &fakeSandbox{out: "Warning: connecting to a deprecated thing\n2000\n"}
	eng := (Driver{}).Engine(sb, spec.Source{Database: "shop"})
	n, err := eng.Count(ctx, "db.ledger.countDocuments({})")
	if err != nil || n != 2000 {
		t.Fatalf("Count = %d, %v; want 2000", n, err)
	}
	js := strings.Join(sb.cmds[0], " ")
	if !strings.Contains(js, `db = db.getSiblingDB("shop")`) {
		t.Errorf("engine did not bind db to the restored database: %s", js)
	}
	if strings.Contains(js, "s3cr3t") {
		t.Errorf("credentials must never reach argv: %s", js)
	}

	bad := (Driver{}).Engine(&fakeSandbox{out: "not a number"}, spec.Source{})
	if _, err := bad.Count(ctx, "db.ledger.stats()"); err == nil {
		t.Error("expected an error when the expression yields no number")
	}

	sb = &fakeSandbox{out: "fd-canary-2f8a91c4\n"}
	got, err := (Driver{}).Engine(sb, spec.Source{}).Scalar(ctx, "db.c.findOne().token")
	if err != nil || got != "fd-canary-2f8a91c4" {
		t.Fatalf("Scalar = %q, %v", got, err)
	}

	sb = &fakeSandbox{out: "3\n"}
	rows, err := (Driver{}).Engine(sb, spec.Source{}).Rows(ctx, "db.ledger.find({}).limit(3)")
	if err != nil || rows != 3 {
		t.Fatalf("Rows = %d, %v; want 3", rows, err)
	}

	// A non-zero exit is an error, never a passing check.
	sb = &fakeSandbox{out: "MongoServerError: Authentication failed", code: 1}
	if _, err := (Driver{}).Engine(sb, spec.Source{}).Count(ctx, "db.x.countDocuments({})"); err == nil {
		t.Error("expected an error when mongosh exits non-zero")
	}
}

func TestEngineChecksumHashesInsideTheSandbox(t *testing.T) {
	sb := &fakeSandbox{out: "c0710d6b4f15dfa88f600b0e6b624077\n"}
	sum, err := (Driver{}).Engine(sb, spec.Source{Database: "shop"}).Checksum(context.Background(), "ledger", "id")
	if err != nil || sum != "c0710d6b4f15dfa88f600b0e6b624077" {
		t.Fatalf("Checksum = %q, %v", sum, err)
	}
	script := strings.Join(sb.cmds[0], " ")
	// Values are sorted and hashed in the container, so a large collection
	// never crosses the exec boundary.
	for _, want := range []string{"md5sum", `.sort({"id": 1})`, `getSiblingDB("shop")`} {
		if !strings.Contains(script, want) {
			t.Errorf("checksum script missing %q:\n%s", want, script)
		}
	}
}

func TestEngineDefaultsToSandboxDatabase(t *testing.T) {
	sb := &fakeSandbox{out: "1\n"}
	if _, err := (Driver{}).Engine(sb, spec.Source{}).Count(context.Background(), "1"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(sb.cmds[0], " "), `getSiblingDB("firedrill")`) {
		t.Errorf("expected the sandbox database as the default: %v", sb.cmds[0])
	}
}

func TestRestoreExcludesTheSourceUserCatalog(t *testing.T) {
	// Restoring admin.* over the sandbox's own root user would lock the
	// drill out of the data it just restored.
	sb := &fakeSandbox{}
	f := t.TempDir() + "/archive"
	if err := writeFile(f); err != nil {
		t.Fatal(err)
	}
	if _, err := (Driver{}).Restore(context.Background(), sb, f, spec.Source{}); err != nil {
		t.Fatalf("restore: %v", err)
	}
	restore := strings.Join(sb.cmds[len(sb.cmds)-1], " ")
	for _, want := range []string{"mongorestore", "--nsExclude admin.*", "--nsExclude config.*", "--config " + toolsConfigPath} {
		if !strings.Contains(restore, want) {
			t.Errorf("restore command missing %q:\n%s", want, restore)
		}
	}
	if strings.Contains(restore, "s3cr3t") {
		t.Errorf("password must not appear in argv: %s", restore)
	}
}

func TestPreflightRejectsAnImageWithoutTools(t *testing.T) {
	sb := &fakeSandbox{code: 127}
	f := t.TempDir() + "/archive"
	if err := writeFile(f); err != nil {
		t.Fatal(err)
	}
	_, err := (Driver{}).Restore(context.Background(), sb, f, spec.Source{})
	if err == nil || !strings.Contains(err.Error(), "mongorestore") {
		t.Fatalf("expected an actionable tooling error, got %v", err)
	}
}

func TestQuoting(t *testing.T) {
	if got := quoteJS(`a"b\c`); got != `"a\"b\\c"` {
		t.Errorf("quoteJS = %s", got)
	}
	if got := shellQuote("it's"); got != `'it'\''s'` {
		t.Errorf("shellQuote = %s", got)
	}
	if got := lastLine("notice\n\nvalue\n"); got != "value" {
		t.Errorf("lastLine = %q", got)
	}
}

func writeFile(path string) error {
	return os.WriteFile(path, []byte("archive"), 0o600)
}
