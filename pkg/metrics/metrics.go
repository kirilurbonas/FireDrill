// Package metrics exports drill results as Prometheus metrics. The CLI is a
// short-lived batch job, so the two supported patterns are the node_exporter
// textfile collector (write a .prom file) and the Pushgateway (push over
// HTTP). Sink failures never fail a drill.
package metrics

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/push"
	"github.com/prometheus/common/expfmt"

	"github.com/kirilurbonas/FireDrill/pkg/report"
	"github.com/kirilurbonas/FireDrill/pkg/spec"
)

// Export sends the evidence to every configured sink and returns one error
// per failed sink (callers should treat them as warnings).
func Export(e *report.Evidence, sinks []spec.Sink) []error {
	if len(sinks) == 0 {
		return nil
	}
	var errs []error
	for _, s := range sinks {
		var err error
		switch s.Type {
		case "prometheus":
			err = writeTextfile(Registry(e, true), s.TextfileDir, e.Drill)
		case "pushgateway":
			// The grouping key supplies the drill label; metrics must not
			// repeat it or the Pushgateway rejects the push.
			err = push.New(s.URL, "firedrill").
				Grouping("drill", e.Drill).
				Gatherer(Registry(e, false)).
				Push()
		default:
			err = fmt.Errorf("unsupported sink type %q", s.Type)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("sink %s: %w", s.Type, err))
		}
	}
	return errs
}

// Registry builds a fresh registry populated with the drill's metrics.
// withDrillLabel controls whether metrics carry the drill label themselves
// (textfile) or inherit it from the push grouping key (pushgateway).
func Registry(e *report.Evidence, withDrillLabel bool) *prometheus.Registry {
	reg := prometheus.NewRegistry()
	base := prometheus.Labels{}
	labelNames := []string{}
	if withDrillLabel {
		base["drill"] = e.Drill
		labelNames = []string{"drill"}
	}

	gauge := func(name, help string, v float64) {
		g := prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: name, Help: help}, labelNames)
		reg.MustRegister(g)
		g.With(base).Set(v)
	}
	b2f := func(b bool) float64 {
		if b {
			return 1
		}
		return 0
	}

	gauge("firedrill_drill_verified", "1 if the drill verified recovery end to end.", b2f(e.Verified))
	gauge("firedrill_restore_duration_seconds", "Measured restore time (the drill's RTO).", e.Measured.RestoreSeconds)
	gauge("firedrill_backup_age_seconds", "Age of the backup at drill time (the drill's RPO).", e.Backup.AgeSecs)
	gauge("firedrill_rto_met", "1 if the measured restore time met the RTO objective.", b2f(e.Measured.RTOMet))
	gauge("firedrill_rpo_met", "1 if the backup age met the RPO objective.", b2f(e.Measured.RPOMet))
	gauge("firedrill_drill_timestamp_seconds", "Unix time the drill finished.", float64(e.FinishedAt.Unix()))

	check := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "firedrill_check_passed",
		Help: "1 if the check passed, 0 if it failed or was skipped.",
	}, append(labelNames, "check"))
	reg.MustRegister(check)
	for _, r := range e.Checks {
		l := prometheus.Labels{"check": r.Name}
		for k, v := range base {
			l[k] = v
		}
		check.With(l).Set(b2f(r.Passed))
	}
	return reg
}

// writeTextfile renders the registry in exposition format to
// <dir>/firedrill-<drill>.prom, atomically (write temp + rename) so the
// node_exporter never scrapes a half-written file.
func writeTextfile(reg *prometheus.Registry, dir, drill string) error {
	mfs, err := reg.Gather()
	if err != nil {
		return err
	}
	var b strings.Builder
	enc := expfmt.NewEncoder(&b, expfmt.NewFormat(expfmt.TypeTextPlain))
	for _, mf := range mfs {
		if err := enc.Encode(mf); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	final := filepath.Join(dir, fmt.Sprintf("firedrill-%s.prom", drill))
	tmp := final + ".tmp"
	// #nosec G306 -- metrics files are meant to be readable by the node_exporter
	if err := os.WriteFile(tmp, []byte(b.String()), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, final)
}
