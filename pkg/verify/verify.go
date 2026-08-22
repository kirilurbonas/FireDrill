// Package verify runs the drill's verification checks against the restored
// sandbox database. Checks prove the data came back — not just that a
// restore process exited zero.
package verify

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"k8s.io/client-go/kubernetes"

	"github.com/kirilurbonas/FireDrill/pkg/spec"
)

// Context carries drill-level facts checks may need.
type Context struct {
	RestoreErr error         // nil if the restore succeeded
	BackupAge  time.Duration // now - backup mod time
	RTO        time.Duration // objective, for reporting only
	// K8s + Namespace are set for velero drills; K8s checks run against
	// the restored ephemeral namespace.
	K8s       kubernetes.Interface
	Namespace string
}

// Result is the outcome of one check.
type Result struct {
	Name    string `json:"name"`
	Passed  bool   `json:"passed"`
	Detail  string `json:"detail"`
	Skipped bool   `json:"skipped,omitempty"`
}

// Run executes every configured check in order against the restored engine
// (nil for drill kinds with no queryable engine, e.g. velero). If the restore
// failed, data checks are reported as skipped rather than misleading failures.
func Run(ctx context.Context, eng Engine, checks []spec.Check, dc Context) []Result {
	results := make([]Result, 0, len(checks))
	for _, c := range checks {
		results = append(results, runOne(ctx, eng, c, dc))
	}
	return results
}

func runOne(ctx context.Context, eng Engine, c spec.Check, dc Context) Result {
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
			return requireEngine(eng, "rowCount", func() Result {
				n, err := eng.Count(ctx, c.RowCount.Query)
				if err != nil {
					return Result{Name: "rowCount", Passed: false, Detail: "query failed: " + err.Error()}
				}
				return Result{
					Name:   "rowCount",
					Passed: n >= c.RowCount.Min,
					Detail: fmt.Sprintf("%d rows (min %d)", n, c.RowCount.Min),
				}
			})
		})

	case c.Checksum != nil:
		return dataCheck(dc, "checksum", func() Result {
			return requireEngine(eng, "checksum", func() Result { return checksum(ctx, eng, c.Checksum) })
		})

	case c.Smoke != nil:
		return dataCheck(dc, "smoke", func() Result {
			return requireEngine(eng, "smoke", func() Result { return smoke(ctx, eng, c.Smoke) })
		})

	case c.Canary != nil:
		return dataCheck(dc, "canary", func() Result {
			return requireEngine(eng, "canary", func() Result { return canary(ctx, eng, c.Canary) })
		})

	case c.PodsReady != nil:
		return dataCheck(dc, "podsReady", func() Result { return podsReady(ctx, dc.K8s, dc.Namespace, c.PodsReady) })

	case c.ResourceCount != nil:
		return dataCheck(dc, "resourceCount", func() Result { return resourceCount(ctx, dc.K8s, dc.Namespace, c.ResourceCount) })
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

// requireEngine guards data checks against a missing engine — a panic here
// inside the operator would kill the reconciler and leak the sandbox.
func requireEngine(eng Engine, name string, run func() Result) Result {
	if eng == nil {
		return Result{Name: name, Passed: false, Detail: "no database connection configured for this drill type"}
	}
	return run()
}

var identRe = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_.]*$`)

// checksum computes an order-independent checksum over one column of a
// table, using the engine's dialect. Identifiers are validated (they cannot
// be bound as query params) before they reach the engine.
func checksum(ctx context.Context, eng Engine, c *spec.ChecksumCheck) Result {
	if !identRe.MatchString(c.Table) || !identRe.MatchString(c.Column) {
		return Result{Name: "checksum", Passed: false, Detail: "invalid table/column identifier"}
	}
	sum, err := eng.Checksum(ctx, c.Table, c.Column)
	if err != nil {
		if errors.Is(err, errNoChecksumDialect) {
			return Result{Name: "checksum", Passed: false, Detail: errNoChecksumDialect.Error()}
		}
		return Result{Name: "checksum", Passed: false, Detail: "query failed: " + err.Error()}
	}
	if c.Expect != "" && sum != c.Expect {
		return Result{Name: "checksum", Passed: false,
			Detail: fmt.Sprintf("%s.%s = %s, expected %s", c.Table, c.Column, sum, c.Expect)}
	}
	return Result{Name: "checksum", Passed: true, Detail: fmt.Sprintf("%s.%s md5=%s", c.Table, c.Column, sum)}
}

// smoke runs a user query and asserts on the number of returned rows.
func smoke(ctx context.Context, eng Engine, c *spec.SmokeCheck) Result {
	n, err := eng.Rows(ctx, c.Statement())
	if err != nil {
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

// canary asserts a planted sentinel value restored byte-exact. Ransomware-
// encrypted or truncated backups cannot reproduce a known token, so this
// catches corruption that row counts and freshness never would. The exact
// value is never written to results — evidence must not leak the sentinel.
func canary(ctx context.Context, eng Engine, c *spec.CanaryCheck) Result {
	got, err := eng.Scalar(ctx, c.Statement())
	if err != nil {
		return Result{Name: "canary", Passed: false, Detail: "query failed: " + err.Error()}
	}
	if got != c.Expect {
		return Result{Name: "canary", Passed: false, Detail: "sentinel value mismatch — possible ransomware/corruption"}
	}
	return Result{Name: "canary", Passed: true, Detail: "sentinel restored intact"}
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
