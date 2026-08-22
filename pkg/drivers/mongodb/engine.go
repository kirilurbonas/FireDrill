package mongodb

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/kirilurbonas/FireDrill/pkg/drivers"
)

// engine evaluates checks as mongosh expressions inside the sandbox
// container. Check queries are engine-dialect, exactly as SQL checks are:
//
//	rowCount: { query: "db.ledger.countDocuments({})", min: 100000 }
//	smoke:    { query: "db.accounts.find({status: 'active'}).limit(1)" }
//	canary:   { query: "db.firedrill_canary.findOne().token", expect: "…" }
type engine struct {
	sb drivers.Sandbox
	db string // restored database the checks run against
}

// eval runs a mongosh script against the restored database and returns its
// trimmed output. `db` is bound to the drill's database, so user expressions
// read the way they would in a mongosh session.
func (e *engine) eval(ctx context.Context, script string) (string, error) {
	js := authJS + "db = db.getSiblingDB(" + quoteJS(e.db) + ");" + script
	code, out, err := e.sb.Exec(ctx, []string{"mongosh", "--quiet", "--eval", js}, nil)
	if err != nil {
		return "", err
	}
	out = strings.TrimSpace(out)
	if code != 0 {
		return "", fmt.Errorf("mongosh exited %d: %s", code, tail(out, 500))
	}
	return out, nil
}

func (e *engine) Count(ctx context.Context, query string) (int64, error) {
	out, err := e.eval(ctx, "print("+query+");")
	if err != nil {
		return 0, err
	}
	n, err := strconv.ParseInt(lastLine(out), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("expected a number from %q, got %q", query, tail(out, 200))
	}
	return n, nil
}

// Rows counts what the expression returned, accepting the shapes a user
// naturally writes: a cursor (find), an array (aggregate().toArray()), a
// single document (findOne), or null.
func (e *engine) Rows(ctx context.Context, query string) (int, error) {
	out, err := e.eval(ctx, `
const _r = (`+query+`);
if (_r === null || _r === undefined) { print(0); }
else if (Array.isArray(_r)) { print(_r.length); }
else if (typeof _r.itcount === "function") { print(_r.itcount()); }
else if (typeof _r.toArray === "function") { print(_r.toArray().length); }
else if (typeof _r === "number") { print(_r); }
else { print(1); }`)
	if err != nil {
		return 0, err
	}
	n, err := strconv.Atoi(lastLine(out))
	if err != nil {
		return 0, fmt.Errorf("expected a row count from %q, got %q", query, tail(out, 200))
	}
	return n, nil
}

func (e *engine) Scalar(ctx context.Context, query string) (string, error) {
	out, err := e.eval(ctx, "print("+query+");")
	if err != nil {
		return "", err
	}
	return lastLine(out), nil
}

// Checksum is an order-independent md5 over one field of one collection:
// values are printed in sorted order and hashed inside the container, so a
// large collection never crosses the exec boundary. This mirrors the SQL
// drivers' md5-over-sorted-values, so the semantic is the same everywhere.
func (e *engine) Checksum(ctx context.Context, collection, field string) (string, error) {
	js := authJS + "db = db.getSiblingDB(" + quoteJS(e.db) + ");" +
		"db.getCollection(" + quoteJS(collection) + ").find({}, {_id: 0, " + quoteJS(field) + ": 1})" +
		".sort({" + quoteJS(field) + ": 1})" +
		".forEach(d => print(String(d[" + quoteJS(field) + "])));"

	// `set -o pipefail` is not portable to the image's /bin/sh, so mongosh's
	// exit status is checked explicitly before the hash is trusted.
	script := `out=$(mongosh --quiet --eval "$FIREDRILL_JS") || { echo "$out"; exit 1; }
if [ -z "$out" ]; then echo empty; else printf '%s' "$out" | md5sum | cut -d' ' -f1; fi`

	code, out, err := e.sb.Exec(ctx, []string{"sh", "-c",
		"FIREDRILL_JS=" + shellQuote(js) + "; " + script}, nil)
	if err != nil {
		return "", err
	}
	out = strings.TrimSpace(out)
	if code != 0 {
		return "", fmt.Errorf("checksum failed (exit %d): %s", code, tail(out, 500))
	}
	return lastLine(out), nil
}

// lastLine is the value line: mongosh may emit a deprecation or connection
// notice before the script's own output.
func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

// shellQuote renders a string as a single-quoted shell word.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
