package report

import (
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

// GateOptions configures a recovery-SLO check over an evidence directory.
type GateOptions struct {
	Dir string // evidence directory
	// MaxAge is how old the most recent *verified* run may be. 0 disables
	// the staleness check.
	MaxAge time.Duration
	// By selects what a subject is: "drill" (default) or "control".
	By string
	// Subjects are the drills/controls that must be evidenced. Empty means
	// "whatever the evidence directory happens to contain" — useful for a
	// quick look, useless as a guarantee, because a drill that stopped
	// running entirely leaves nothing behind to check.
	Subjects []string
	// RequireSigned fails a subject whose evidence carries no valid signature.
	RequireSigned bool
	// PublicKey, when set, additionally pins the signer: evidence signed by
	// any other key does not count. Without it a signature only proves the
	// evidence was not tampered with after the fact.
	PublicKey ed25519.PublicKey
	// AllowUnverified accepts a subject whose latest run failed, as long as
	// a verified run still falls inside MaxAge.
	AllowUnverified bool
	// Now is injectable for tests; zero means time.Now().
	Now time.Time
}

// SubjectStatus is one drill's or control's standing against the gate.
type SubjectStatus struct {
	Subject      string     `json:"subject"`
	Runs         int        `json:"runs"`
	LastRun      *time.Time `json:"lastRun,omitempty"`
	LastVerified *time.Time `json:"lastVerified,omitempty"`
	Signed       bool       `json:"signed"`
	// Violations is empty when the subject passes the gate.
	Violations []string `json:"violations,omitempty"`
	Evidence   string   `json:"evidence,omitempty"` // latest evidence file
}

// OK reports whether the subject satisfies the gate.
func (s SubjectStatus) OK() bool { return len(s.Violations) == 0 }

// GateReport is the outcome of one gate evaluation.
type GateReport struct {
	GeneratedAt time.Time       `json:"generatedAt"`
	EvidenceDir string          `json:"evidenceDir"`
	By          string          `json:"by"`
	MaxAge      string          `json:"maxAge,omitempty"`
	Subjects    []SubjectStatus `json:"subjects"`
	Violations  int             `json:"violations"`
}

// Gate answers the question `controls` cannot: is recovery still being
// proven? It reports a violation when a subject has no evidence at all
// (a drill that quietly stopped running), when its most recent verified run
// has aged out of the objective, when its latest run failed, or — with
// RequireSigned — when that evidence is not signed.
func Gate(opts GateOptions) (*GateReport, error) {
	files, err := scanEvidence(opts.Dir)
	if err != nil {
		return nil, err
	}
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	by := opts.By
	if by == "" {
		by = "drill"
	}
	if by != "drill" && by != "control" {
		return nil, fmt.Errorf("gate by %q unsupported (drill|control)", by)
	}

	// Group evidence by subject.
	groups := map[string][]evidenceFile{}
	for _, f := range files {
		for _, s := range subjectsOf(f.E, by) {
			groups[s] = append(groups[s], f)
		}
	}

	// Required subjects come from the caller when given, so a drill that
	// disappeared is still checked; otherwise report on what exists.
	names := opts.Subjects
	if len(names) == 0 {
		for s := range groups {
			names = append(names, s)
		}
	}
	sort.Strings(names)

	rep := &GateReport{
		GeneratedAt: now.UTC(),
		EvidenceDir: opts.Dir,
		By:          by,
	}
	if opts.MaxAge > 0 {
		rep.MaxAge = opts.MaxAge.String()
	}
	for _, name := range names {
		st := evaluate(name, groups[name], opts, now)
		rep.Subjects = append(rep.Subjects, st)
		if !st.OK() {
			rep.Violations++
		}
	}
	return rep, nil
}

func subjectsOf(e Evidence, by string) []string {
	if by == "control" {
		return e.Controls
	}
	return []string{e.Drill}
}

func evaluate(name string, runs []evidenceFile, opts GateOptions, now time.Time) SubjectStatus {
	st := SubjectStatus{Subject: name, Runs: len(runs)}
	if len(runs) == 0 {
		st.Violations = append(st.Violations, "no evidence — has this drill run at all?")
		return st
	}
	sort.Slice(runs, func(i, j int) bool { return runs[i].E.FinishedAt.After(runs[j].E.FinishedAt) })

	latest := runs[0]
	st.LastRun = &latest.E.FinishedAt
	st.Evidence = latest.Path

	var verified *evidenceFile
	for i := range runs {
		if runs[i].E.Verified {
			verified = &runs[i]
			break
		}
	}
	if verified != nil {
		st.LastVerified = &verified.E.FinishedAt
	}

	if !latest.E.Verified && !opts.AllowUnverified {
		st.Violations = append(st.Violations, "latest run did not verify recovery")
	}
	if opts.MaxAge > 0 {
		switch {
		case verified == nil:
			st.Violations = append(st.Violations, "no verified run on record")
		case now.Sub(verified.E.FinishedAt) > opts.MaxAge:
			st.Violations = append(st.Violations,
				fmt.Sprintf("last verified run was %s ago (max %s)",
					humanDur(now.Sub(verified.E.FinishedAt)), humanDur(opts.MaxAge)))
		}
	}
	// Signature status describes the run the gate leans on.
	sigOn := latest
	if verified != nil {
		sigOn = *verified
	}
	st.Signed = Verify(sigOn.Path, opts.PublicKey) == nil
	if opts.RequireSigned && !st.Signed {
		msg := "evidence is unsigned or its signature does not validate"
		if opts.PublicKey != nil {
			msg += " against the pinned key"
		}
		st.Violations = append(st.Violations, msg)
	}
	return st
}

// WriteText renders the gate result as a terminal table.
func (r *GateReport) WriteText(w io.Writer) {
	if len(r.Subjects) == 0 {
		fmt.Fprintf(w, "no %ss to check in %s\n", r.By, r.EvidenceDir)
		return
	}
	fmt.Fprintf(w, "%-24s  %-17s  %-17s  %-6s  %s\n",
		strings.ToUpper(r.By), "LAST RUN (UTC)", "LAST VERIFIED", "SIGNED", "STATUS")
	for _, s := range r.Subjects {
		status := "ok"
		if !s.OK() {
			status = "FAIL: " + strings.Join(s.Violations, "; ")
		}
		fmt.Fprintf(w, "%-24s  %-17s  %-17s  %-6s  %s\n",
			trunc(s.Subject, 24), stamp(s.LastRun), stamp(s.LastVerified), yes(s.Signed), status)
	}
	fmt.Fprintf(w, "\n%d %s(s): %d ok, %d failing\n",
		len(r.Subjects), r.By, len(r.Subjects)-r.Violations, r.Violations)
}

// WriteJSON renders the machine-readable gate result.
func (r *GateReport) WriteJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

// humanDur renders an age the way an operator would say it — "29d", "6h" —
// rather than 700h0m0s.
func humanDur(d time.Duration) string {
	switch {
	case d >= 48*time.Hour:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	case d >= 2*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	case d >= time.Minute:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return d.Round(time.Second).String()
	}
}

func stamp(t *time.Time) string {
	if t == nil {
		return "—"
	}
	return t.UTC().Format("2006-01-02 15:04")
}
