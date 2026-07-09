// Package verify runs the drill's verification checks against the restored
// sandbox database. Checks prove the data came back — not just that a
// restore process exited zero.
package verify

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/kirilurbonas/FireDrill/pkg/spec"
)

// Context carries drill-level facts checks may need.
type Context struct {
	RestoreErr error         // nil if the restore succeeded
	BackupAge  time.Duration // now - backup mod time
	RTO        time.Duration // objective, for reporting only
	// ChecksumQuery builds the engine-dialect checksum query. Identifiers
	// are validated before this is called.
	ChecksumQuery func(table, column string) string
}

// Result is the outcome of one check.
type Result struct {
	Name    string `json:"name"`
	Passed  bool   `json:"passed"`
	Detail  string `json:"detail"`
	Skipped bool   `json:"skipped,omitempty"`
}

// Run executes every configured check in order. If the restore failed, data
// checks are reported as skipped rather than misleading failures.
func Run(ctx context.Context, db *sql.DB, checks []spec.Check, dc Context) []Result {
	results := make([]Result, 0, len(checks))
	for _, c := range checks {
		results = append(results, runOne(ctx, db, c, dc))
	}
	return results
}

func runOne(ctx context.Context, db *sql.DB, c spec.Check, dc Context) Result {
	switch {
	case c.RestoreSucceeded != nil:
		if dc.RestoreErr != nil {
			return Result{Name: "restoreSucceeded", Passed: false, Detail: dc.RestoreErr.Error()}
		}
		return Result{Name: "restoreSucceeded", Passed: true, Detail: "restore completed"}

	case c.Freshness != nil:
		passed := dc.BackupAge <= c.Freshness.MaxAge.Duration
		return Result{
			Name:   "freshness",
			Passed: passed,
			Detail: fmt.Sprintf("backup age %s (max %s)", dc.BackupAge.Round(time.Second), c.Freshness.MaxAge),
		}

	case c.RowCount != nil:
		return dataCheck(dc, "rowCount", func() Result {
			var n int64
			if err := db.QueryRowContext(ctx, c.RowCount.Query).Scan(&n); err != nil {
				return Result{Name: "rowCount", Passed: false, Detail: "query failed: " + err.Error()}
			}
			return Result{
				Name:   "rowCount",
				Passed: n >= c.RowCount.Min,
				Detail: fmt.Sprintf("%d rows (min %d)", n, c.RowCount.Min),
			}
		})

	case c.Checksum != nil:
		return dataCheck(dc, "checksum", func() Result { return checksum(ctx, db, c.Checksum, dc.ChecksumQuery) })

	case c.Smoke != nil:
		return dataCheck(dc, "smoke", func() Result { return smoke(ctx, db, c.Smoke) })
	}
	return Result{Name: "unknown", Passed: false, Detail: "unrecognized check"}
}

// dataCheck skips DB-dependent checks when the restore itself failed.
func dataCheck(dc Context, name string, run func() Result) Result {
	if dc.RestoreErr != nil {
		return Result{Name: name, Skipped: true, Detail: "skipped: restore failed"}
	}
	return run()
}

var identRe = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_.]*$`)

// checksum computes an order-independent checksum over one column of a
// table, using the driver's dialect. Identifiers are validated (they cannot
// be bound as SQL params) before interpolation.
func checksum(ctx context.Context, db *sql.DB, c *spec.ChecksumCheck, query func(table, column string) string) Result {
	if !identRe.MatchString(c.Table) || !identRe.MatchString(c.Column) {
		return Result{Name: "checksum", Passed: false, Detail: "invalid table/column identifier"}
	}
	if query == nil {
		return Result{Name: "checksum", Passed: false, Detail: "no checksum dialect configured"}
	}
	q := query(c.Table, c.Column)
	var sum string
	if err := db.QueryRowContext(ctx, q).Scan(&sum); err != nil {
		return Result{Name: "checksum", Passed: false, Detail: "query failed: " + err.Error()}
	}
	if c.Expect != "" && sum != c.Expect {
		return Result{Name: "checksum", Passed: false,
			Detail: fmt.Sprintf("%s.%s = %s, expected %s", c.Table, c.Column, sum, c.Expect)}
	}
	return Result{Name: "checksum", Passed: true, Detail: fmt.Sprintf("%s.%s md5=%s", c.Table, c.Column, sum)}
}

// smoke runs a user query and asserts on the number of returned rows.
func smoke(ctx context.Context, db *sql.DB, c *spec.SmokeCheck) Result {
	rows, err := db.QueryContext(ctx, c.SQL)
	if err != nil {
		return Result{Name: "smoke", Passed: false, Detail: "query failed: " + err.Error()}
	}
	defer func() { _ = rows.Close() }()
	n := 0
	for rows.Next() {
		n++
	}
	if err := rows.Err(); err != nil {
		return Result{Name: "smoke", Passed: false, Detail: "query failed: " + err.Error()}
	}
	expect := c.ExpectRows
	if expect == "" {
		expect = ">=1"
	}
	ok, err := evalRows(n, expect)
	if err != nil {
		return Result{Name: "smoke", Passed: false, Detail: err.Error()}
	}
	return Result{Name: "smoke", Passed: ok, Detail: fmt.Sprintf("%d rows (expect %s)", n, expect)}
}

var rowsExprRe = regexp.MustCompile(`^(>=|<=|==|>|<)\s*(\d+)$`)

func evalRows(n int, expr string) (bool, error) {
	m := rowsExprRe.FindStringSubmatch(strings.TrimSpace(expr))
	if m == nil {
		return false, fmt.Errorf("invalid expectRows %q (want e.g. \">=1\")", expr)
	}
	v, _ := strconv.Atoi(m[2])
	switch m[1] {
	case ">=":
		return n >= v, nil
	case "<=":
		return n <= v, nil
	case "==":
		return n == v, nil
	case ">":
		return n > v, nil
	case "<":
		return n < v, nil
	}
	return false, nil
}
