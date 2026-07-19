// Package spec defines the RecoveryDrill document and its YAML loader.
package spec

import (
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// nameRe: drill names appear in evidence filenames, container names and pod
// names, so they must be safe lowercase DNS labels.
var nameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

const (
	APIVersion = "firedrill.dev/v1"
	Kind       = "RecoveryDrill"
)

// Drill is a parsed, validated RecoveryDrill document.
type Drill struct {
	APIVersion string   `yaml:"apiVersion"`
	Kind       string   `yaml:"kind"`
	Metadata   Metadata `yaml:"metadata"`
	Spec       Spec     `yaml:"spec"`
}

type Metadata struct {
	Name string `yaml:"name"`
}

type Spec struct {
	Schedule   string     `yaml:"schedule,omitempty"` // used by the operator (v0.3); ignored by the CLI
	Objectives Objectives `yaml:"objectives"`
	Source     Source     `yaml:"source"`
	Sandbox    Sandbox    `yaml:"sandbox"`
	Verify     []Check    `yaml:"verify"`
	Report     Report     `yaml:"report"`
}

type Objectives struct {
	RTO Duration `yaml:"rto"` // restore must complete within this
	RPO Duration `yaml:"rpo"` // backup must be younger than this
}

type Source struct {
	Driver string `yaml:"driver"` // postgres | mysql | velero
	From   From   `yaml:"from"`
	// Format of the backup artifact (postgres only):
	//   dump (default)  — pg_dump output, logical restore into a fresh DB
	//   basebackup      — pg_basebackup tar (-Ft -X fetch), physical restore:
	//                     the sandbox starts cold, the data directory is
	//                     untarred in place, and Postgres crash-recovers over it
	Format string `yaml:"format,omitempty"`
	// Database names the DB verification checks connect to for basebackup
	// restores (a physical restore brings back the whole cluster). Default
	// "postgres".
	Database string `yaml:"database,omitempty"`
}

type From struct {
	Type string `yaml:"type"`          // file | s3 | velero
	URI  string `yaml:"uri,omitempty"` // path or s3://bucket/key (file/s3)
	// CredentialsRef names an external credential source (env profile, secret).
	// Credentials are never inlined in the spec.
	CredentialsRef string `yaml:"credentialsRef,omitempty"`
	Region         string `yaml:"region,omitempty"`
	// Endpoint targets S3-compatible stores (MinIO, Ceph, Wasabi, …);
	// path-style addressing is used automatically when set.
	Endpoint string `yaml:"endpoint,omitempty"`
	// MaxBytes aborts the download if the backup exceeds this size —
	// a disk-fill guard for shared runners. 0 = unlimited.
	MaxBytes int64 `yaml:"maxBytes,omitempty"`
	// Velero sources (driver: velero):
	Backup    string `yaml:"backup,omitempty"`    // Velero Backup CR name
	Namespace string `yaml:"namespace,omitempty"` // source namespace the backup covers
}

type Sandbox struct {
	Provider  string   `yaml:"provider"` // docker | kubernetes
	Image     string   `yaml:"image"`
	TTL       Duration `yaml:"ttl"`                 // hard teardown guardrail
	Namespace string   `yaml:"namespace,omitempty"` // kubernetes only; default "firedrill"
}

// Check is a single verification step. Exactly one field must be set.
type Check struct {
	RestoreSucceeded *struct{}       `yaml:"restoreSucceeded,omitempty"`
	Freshness        *FreshnessCheck `yaml:"freshness,omitempty"`
	// SQL checks (engine drivers: postgres, mysql):
	RowCount *RowCountCheck `yaml:"rowCount,omitempty"`
	Checksum *ChecksumCheck `yaml:"checksum,omitempty"`
	Smoke    *SmokeCheck    `yaml:"smoke,omitempty"`
	Canary   *CanaryCheck   `yaml:"canary,omitempty"`
	// Kubernetes checks (velero driver):
	PodsReady     *PodsReadyCheck     `yaml:"podsReady,omitempty"`
	ResourceCount *ResourceCountCheck `yaml:"resourceCount,omitempty"`
}

type FreshnessCheck struct {
	MaxAge Duration `yaml:"maxAge"`
}

type RowCountCheck struct {
	Query string `yaml:"query"`
	Min   int64  `yaml:"min"`
}

type ChecksumCheck struct {
	Table  string `yaml:"table"`
	Column string `yaml:"column"`
	// Expect pins the checksum to a known value; empty means record-only.
	Expect string `yaml:"expect,omitempty"`
}

// CanaryCheck proves a known sentinel value planted before the backup
// restored byte-exact — encrypted-at-source (ransomware) or silently
// corrupted backups cannot reproduce it.
type CanaryCheck struct {
	SQL    string `yaml:"sql"`    // must return exactly one row, one column
	Expect string `yaml:"expect"` // exact expected value
}

type PodsReadyCheck struct {
	Timeout Duration `yaml:"timeout"` // how long pods get to become Ready
}

type ResourceCountCheck struct {
	Kind string `yaml:"kind"` // deployments | statefulsets | services | configmaps | secrets | pods
	Min  int    `yaml:"min"`
}

type SmokeCheck struct {
	SQL        string `yaml:"sql"`
	ExpectRows string `yaml:"expectRows,omitempty"` // e.g. ">=1", "==0"; default ">=1"
}

type Report struct {
	Sign     bool     `yaml:"sign"`
	HTML     bool     `yaml:"html,omitempty"` // also write <evidence>.html
	Controls []string `yaml:"controls,omitempty"`
	Dir      string   `yaml:"dir,omitempty"` // default ./evidence
	Sinks    []Sink   `yaml:"sinks,omitempty"`
}

// Sink is a destination the drill result is exported to after the evidence
// is written. Sink failures are warnings, never drill failures.
type Sink struct {
	Type        string `yaml:"type"`                  // prometheus | pushgateway | slack
	TextfileDir string `yaml:"textfileDir,omitempty"` // prometheus: node_exporter textfile-collector dir
	URL         string `yaml:"url,omitempty"`         // pushgateway: base URL, e.g. http://pushgw:9091
	// WebhookEnv names the environment variable holding the Slack incoming
	// webhook URL. The URL itself is a secret and never appears in specs.
	WebhookEnv string `yaml:"webhookEnv,omitempty"`
	// OnlyFailures suppresses notifications for verified drills.
	OnlyFailures bool `yaml:"onlyFailures,omitempty"`
}

// Duration wraps time.Duration with YAML parsing for values like "15m".
type Duration struct{ time.Duration }

func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	var s string
	if err := node.Decode(&s); err != nil {
		return err
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration{v}
	return nil
}

func (d Duration) MarshalYAML() (any, error) { return d.String(), nil }

// Load reads a single-drill file (the first document; errors if absent).
func Load(path string) (*Drill, error) {
	drills, err := LoadAll(path)
	if err != nil {
		return nil, err
	}
	return drills[0], nil
}

// LoadAll reads, decodes (strictly) and validates every YAML document in a
// drill file. Duplicate drill names are rejected.
func LoadAll(path string) ([]*Drill, error) {
	f, err := os.Open(path) // #nosec G304 -- user-supplied spec path is the CLI's input
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return ParseAll(f)
}

// Parse decodes a single drill document from r with unknown fields rejected.
func Parse(r io.Reader) (*Drill, error) {
	drills, err := ParseAll(r)
	if err != nil {
		return nil, err
	}
	return drills[0], nil
}

// ParseAll decodes every YAML document from r, validating each.
func ParseAll(r io.Reader) ([]*Drill, error) {
	dec := yaml.NewDecoder(r)
	dec.KnownFields(true)
	var drills []*Drill
	seen := map[string]bool{}
	for {
		var d Drill
		err := dec.Decode(&d)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("parsing drill spec (document %d): %w", len(drills)+1, err)
		}
		if err := d.Validate(); err != nil {
			return nil, fmt.Errorf("document %d (%s): %w", len(drills)+1, d.Metadata.Name, err)
		}
		if seen[d.Metadata.Name] {
			return nil, fmt.Errorf("duplicate drill name %q", d.Metadata.Name)
		}
		seen[d.Metadata.Name] = true
		drills = append(drills, &d)
	}
	if len(drills) == 0 {
		return nil, errors.New("no drill documents found")
	}
	return drills, nil
}

// FindDrill returns the named drill from a set, with a helpful error.
func FindDrill(drills []*Drill, name string) (*Drill, error) {
	var names []string
	for _, d := range drills {
		if d.Metadata.Name == name {
			return d, nil
		}
		names = append(names, d.Metadata.Name)
	}
	return nil, fmt.Errorf("drill %q not found (file contains: %s)", name, strings.Join(names, ", "))
}

// Validate checks structural invariants of the drill.
func (d *Drill) Validate() error {
	var errs []error
	add := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	if d.APIVersion != APIVersion {
		add("apiVersion must be %q, got %q", APIVersion, d.APIVersion)
	}
	if d.Kind != Kind {
		add("kind must be %q, got %q", Kind, d.Kind)
	}
	if !nameRe.MatchString(d.Metadata.Name) {
		add("metadata.name %q must be a lowercase DNS label (a-z, 0-9, '-'; max 63 chars) — it is used in file, container and pod names", d.Metadata.Name)
	}
	velero := d.Spec.Source.Driver == "velero"
	switch d.Spec.Source.Format {
	case "", "dump":
	case "basebackup":
		if d.Spec.Source.Driver != "postgres" {
			add("spec.source.format basebackup is only supported for driver: postgres")
		}
	default:
		add("spec.source.format must be dump or basebackup, got %q", d.Spec.Source.Format)
	}
	if d.Spec.Source.Database != "" && d.Spec.Source.Format != "basebackup" {
		add("spec.source.database is only valid with format: basebackup")
	}
	switch d.Spec.Source.Driver {
	case "postgres", "mysql", "velero":
	default:
		add("spec.source.driver: unsupported driver %q (supported: postgres, mysql, velero)", d.Spec.Source.Driver)
	}
	switch d.Spec.Source.From.Type {
	case "file", "s3":
		if velero {
			add("spec.source.from.type must be velero when driver is velero")
		}
		if d.Spec.Source.From.URI == "" {
			add("spec.source.from.uri is required")
		}
		if d.Spec.Source.From.Endpoint != "" && d.Spec.Source.From.Type != "s3" {
			add("spec.source.from.endpoint is only valid with type: s3")
		}
	case "velero":
		if !velero {
			add("spec.source.from.type velero requires driver: velero")
		}
		if d.Spec.Source.From.Backup == "" {
			add("spec.source.from.backup (Velero Backup name) is required")
		}
		if d.Spec.Source.From.Namespace == "" {
			add("spec.source.from.namespace (source namespace) is required")
		}
	default:
		add("spec.source.from.type must be file, s3 or velero, got %q", d.Spec.Source.From.Type)
	}
	switch d.Spec.Sandbox.Provider {
	case "docker", "kubernetes":
		if velero && d.Spec.Sandbox.Provider != "kubernetes" {
			add("spec.sandbox.provider must be kubernetes for velero drills")
		}
	default:
		add("spec.sandbox.provider: unsupported provider %q (supported: docker, kubernetes)", d.Spec.Sandbox.Provider)
	}
	if d.Spec.Sandbox.Image == "" && !velero {
		add("spec.sandbox.image is required")
	}
	if d.Spec.Sandbox.TTL.Duration <= 0 {
		add("spec.sandbox.ttl must be > 0 (hard teardown guardrail)")
	}
	if d.Spec.Objectives.RTO.Duration <= 0 {
		add("spec.objectives.rto must be > 0")
	}
	if d.Spec.Objectives.RPO.Duration <= 0 {
		add("spec.objectives.rpo must be > 0")
	}
	if len(d.Spec.Verify) == 0 {
		add("spec.verify must contain at least one check")
	}
	// A drill that never inspects restored data can "verify" an empty
	// restore — the worst possible false positive. Require at least one
	// data-proving check.
	hasDataCheck := false
	for _, c := range d.Spec.Verify {
		if c.provesData() {
			hasDataCheck = true
			break
		}
	}
	if len(d.Spec.Verify) > 0 && !hasDataCheck {
		add("spec.verify must include at least one data-proving check (rowCount, checksum, smoke, canary, podsReady or resourceCount) — restoreSucceeded/freshness alone cannot prove the data came back")
	}
	for i, c := range d.Spec.Verify {
		if err := c.validate(); err != nil {
			add("spec.verify[%d]: %w", i, err)
		}
		if velero && c.isSQL() {
			add("spec.verify[%d]: SQL checks are not valid for velero drills (use podsReady/resourceCount)", i)
		}
		if !velero && c.isK8s() {
			add("spec.verify[%d]: kubernetes checks are only valid for velero drills", i)
		}
	}
	for i, s := range d.Spec.Report.Sinks {
		switch s.Type {
		case "prometheus":
			if s.TextfileDir == "" {
				add("spec.report.sinks[%d]: prometheus sink requires textfileDir", i)
			}
		case "pushgateway":
			if s.URL == "" {
				add("spec.report.sinks[%d]: pushgateway sink requires url", i)
			}
		case "slack":
			if s.WebhookEnv == "" {
				add("spec.report.sinks[%d]: slack sink requires webhookEnv (name of the env var holding the webhook URL)", i)
			}
		default:
			add("spec.report.sinks[%d]: unsupported sink type %q (supported: prometheus, pushgateway, slack)", i, s.Type)
		}
	}
	return errors.Join(errs...)
}

func (c *Check) validate() error {
	n := 0
	if c.RestoreSucceeded != nil {
		n++
	}
	if c.Freshness != nil {
		n++
		if c.Freshness.MaxAge.Duration <= 0 {
			return errors.New("freshness.maxAge must be > 0")
		}
	}
	if c.RowCount != nil {
		n++
		if c.RowCount.Query == "" {
			return errors.New("rowCount.query is required")
		}
	}
	if c.Checksum != nil {
		n++
		if c.Checksum.Table == "" || c.Checksum.Column == "" {
			return errors.New("checksum requires table and column")
		}
	}
	if c.Smoke != nil {
		n++
		if c.Smoke.SQL == "" {
			return errors.New("smoke.sql is required")
		}
	}
	if c.Canary != nil {
		n++
		if c.Canary.SQL == "" || c.Canary.Expect == "" {
			return errors.New("canary requires sql and expect")
		}
	}
	if c.PodsReady != nil {
		n++
		if c.PodsReady.Timeout.Duration <= 0 {
			return errors.New("podsReady.timeout must be > 0")
		}
	}
	if c.ResourceCount != nil {
		n++
		switch c.ResourceCount.Kind {
		case "deployments", "statefulsets", "services", "configmaps", "secrets", "pods":
		default:
			return fmt.Errorf("resourceCount.kind %q unsupported (deployments|statefulsets|services|configmaps|secrets|pods)", c.ResourceCount.Kind)
		}
	}
	if n != 1 {
		return fmt.Errorf("exactly one check type must be set, got %d", n)
	}
	return nil
}

// provesData reports whether the check inspects restored data (as opposed
// to process metadata like exit status or backup age).
func (c *Check) provesData() bool {
	return c.isSQL() || c.isK8s()
}

func (c *Check) isSQL() bool {
	return c.RowCount != nil || c.Checksum != nil || c.Smoke != nil || c.Canary != nil
}

func (c *Check) isK8s() bool {
	return c.PodsReady != nil || c.ResourceCount != nil
}
