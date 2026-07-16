// Command firedrill runs recovery drills: it restores real backups into
// disposable sandboxes, verifies the data, and emits signed evidence.
package main

import (
	"crypto/ed25519"
	"fmt"
	"os"

	"github.com/mattn/go-isatty"
	"github.com/spf13/cobra"

	"github.com/kirilurbonas/FireDrill/pkg/drill"
	"github.com/kirilurbonas/FireDrill/pkg/operator"
	"github.com/kirilurbonas/FireDrill/pkg/report"
	"github.com/kirilurbonas/FireDrill/pkg/spec"
	"github.com/kirilurbonas/FireDrill/pkg/version"
)

const (
	exitFailed = 1 // drill executed but recovery not verified
	exitError  = 2 // drill could not execute
)

func main() {
	root := &cobra.Command{
		Use:           "firedrill",
		Short:         "Fire drills for your backups — prove recovery before you need it",
		Version:       version.Version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(runCmd(), validateCmd(), keygenCmd(), verifyEvidenceCmd(), controlsCmd(), historyCmd(), operatorCmd())
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(exitError)
	}
}

func runCmd() *cobra.Command {
	var (
		file        string
		evidenceDir string
		keyDir      string
		noColor     bool
		dryRun      bool
	)
	var runAll bool
	cmd := &cobra.Command{
		Use:   "run [drill-name]",
		Short: "Execute a recovery drill (or every drill in the file with --all)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			drills, err := spec.LoadAll(file)
			if err != nil {
				return err
			}
			p := &report.Printer{
				W:     os.Stdout,
				Color: !noColor && isatty.IsTerminal(os.Stdout.Fd()),
			}
			opts := drill.Options{
				Printer:     p,
				EvidenceDir: evidenceDir,
				KeyDir:      keyDir,
				Version:     version.Version,
			}

			if runAll {
				if len(args) > 0 {
					return fmt.Errorf("--all and a drill name are mutually exclusive")
				}
				if dryRun {
					p.Info("dry run — would run %d drill(s) from %s", len(drills), file)
					return nil
				}
				outcomes := drill.RunAll(cmd.Context(), drills, opts)
				_, failed, errored := report.WriteScorecard(os.Stdout, outcomes)
				if errored > 0 {
					os.Exit(exitError)
				}
				if failed > 0 {
					os.Exit(exitFailed)
				}
				return nil
			}

			if len(args) == 0 {
				return fmt.Errorf("provide a drill name or --all (file contains %d drills)", len(drills))
			}
			d, err := spec.FindDrill(drills, args[0])
			if err != nil {
				return err
			}
			if dryRun {
				p.Info("dry run — would provision %s sandbox %s (ttl %s), restore %s, run %d checks",
					d.Spec.Sandbox.Provider, d.Spec.Sandbox.Image, d.Spec.Sandbox.TTL,
					d.Spec.Source.From.URI, len(d.Spec.Verify))
				return nil
			}
			e, path, err := drill.Run(cmd.Context(), d, opts)
			if err != nil {
				return err
			}
			p.Summary(e, path, d.Spec.Report.Sign)
			if !e.Verified {
				os.Exit(exitFailed)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&runAll, "all", false, "run every drill in the file and print a scorecard")
	cmd.Flags().StringVarP(&file, "file", "f", "firedrill.yaml", "drill spec file")
	cmd.Flags().StringVar(&evidenceDir, "evidence-dir", "", "override evidence output directory")
	cmd.Flags().StringVar(&keyDir, "key-dir", "", "signing key directory (default ~/.config/firedrill)")
	cmd.Flags().BoolVar(&noColor, "no-color", false, "disable colored output")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print the plan without touching Docker")
	return cmd
}

func controlsCmd() *cobra.Command {
	var evidenceDir, format, outPath string
	cmd := &cobra.Command{
		Use:   "controls",
		Short: "Export recovery-testing evidence grouped by compliance control",
		RunE: func(cmd *cobra.Command, args []string) error {
			rep, err := report.BuildControlReport(evidenceDir)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if outPath != "" {
				f, err := os.Create(outPath) // #nosec G304 -- user-designated output path
				if err != nil {
					return err
				}
				defer func() { _ = f.Close() }()
				out = f
			}
			switch format {
			case "markdown", "md":
				return rep.WriteMarkdown(out)
			case "json":
				return rep.WriteJSON(out)
			default:
				return fmt.Errorf("unsupported format %q (markdown|json)", format)
			}
		},
	}
	cmd.Flags().StringVar(&evidenceDir, "evidence-dir", "evidence", "directory of evidence JSON files")
	cmd.Flags().StringVar(&format, "format", "markdown", "output format: markdown | json")
	cmd.Flags().StringVarP(&outPath, "output", "o", "", "write to file instead of stdout")
	return cmd
}

func historyCmd() *cobra.Command {
	var evidenceDir, drillName string
	cmd := &cobra.Command{
		Use:   "history",
		Short: "Show past drill runs and the RTO trend",
		RunE: func(cmd *cobra.Command, args []string) error {
			entries, err := report.LoadHistory(evidenceDir, drillName)
			if err != nil {
				return err
			}
			report.WriteHistory(cmd.OutOrStdout(), entries)
			return nil
		},
	}
	cmd.Flags().StringVar(&evidenceDir, "evidence-dir", "evidence", "directory of evidence JSON files")
	cmd.Flags().StringVar(&drillName, "drill", "", "filter by drill name")
	return cmd
}

func operatorCmd() *cobra.Command {
	var evidenceDir, metricsAddr string
	cmd := &cobra.Command{
		Use:   "operator",
		Short: "Run the Kubernetes operator (reconciles RecoveryDrill resources)",
		RunE: func(cmd *cobra.Command, args []string) error {
			return operator.RunManager(version.Version, evidenceDir, metricsAddr)
		},
	}
	cmd.Flags().StringVar(&evidenceDir, "evidence-dir", "/evidence", "evidence output directory in the operator pod")
	cmd.Flags().StringVar(&metricsAddr, "metrics-bind-address", ":8080", "controller metrics endpoint")
	return cmd
}

func validateCmd() *cobra.Command {
	var file string
	cmd := &cobra.Command{
		Use:   "validate",
		Short: "Validate a drill spec without running it",
		RunE: func(cmd *cobra.Command, args []string) error {
			drills, err := spec.LoadAll(file)
			if err != nil {
				return err
			}
			for _, d := range drills {
				fmt.Printf("✓ drill %q — %d checks, driver %s, sandbox %s (ttl %s)\n",
					d.Metadata.Name, len(d.Spec.Verify), d.Spec.Source.Driver,
					d.Spec.Sandbox.Provider, d.Spec.Sandbox.TTL)
			}
			fmt.Printf("✓ %s is valid — %d drill(s)\n", file, len(drills))
			return nil
		},
	}
	cmd.Flags().StringVarP(&file, "file", "f", "firedrill.yaml", "drill spec file")
	return cmd
}

func keygenCmd() *cobra.Command {
	var dir string
	cmd := &cobra.Command{
		Use:   "keygen",
		Short: "Generate the ed25519 evidence-signing keypair",
		RunE: func(cmd *cobra.Command, args []string) error {
			if dir == "" {
				var err error
				dir, err = report.DefaultKeyDir()
				if err != nil {
					return err
				}
			}
			priv, pub, err := report.GenerateKeypair(dir)
			if err != nil {
				return err
			}
			fmt.Printf("private key: %s\npublic key:  %s\n", priv, pub)
			return nil
		},
	}
	cmd.Flags().StringVar(&dir, "dir", "", "key directory (default ~/.config/firedrill)")
	return cmd
}

func verifyEvidenceCmd() *cobra.Command {
	var pubPath, keyDir string
	cmd := &cobra.Command{
		Use:   "verify-evidence <evidence.json>",
		Short: "Verify the signature and attestation on an evidence file",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var trusted ed25519.PublicKey
			if pubPath != "" {
				data, err := os.ReadFile(pubPath) // #nosec G304 -- user-supplied key path
				if err != nil {
					return err
				}
				trusted, err = report.ParsePublicKeyPEM(data)
				if err != nil {
					return fmt.Errorf("%w: %s", err, pubPath)
				}
			}
			if err := report.Verify(args[0], trusted); err != nil {
				return err
			}
			fmt.Println("✓ signature valid — evidence is intact")

			// Attestation verification needs the public key; fall back to the
			// default key dir when --public-key wasn't given.
			att := trusted
			if att == nil {
				dir := keyDir
				if dir == "" {
					var err error
					dir, err = report.DefaultKeyDir()
					if err != nil {
						return err
					}
				}
				var err error
				att, err = report.LoadPublicKey(dir)
				if err != nil {
					fmt.Println("– attestation not checked (no public key available)")
					return nil
				}
			}
			if _, err := os.Stat(args[0] + ".intoto.jsonl"); err != nil {
				fmt.Println("– no attestation present (pre-v0.6 evidence)")
				return nil
			}
			if err := report.VerifyAttestation(args[0], att); err != nil {
				return err
			}
			fmt.Println("✓ attestation valid (in-toto/DSSE)")
			return nil
		},
	}
	cmd.Flags().StringVar(&pubPath, "public-key", "", "require signature from this public key (PEM)")
	cmd.Flags().StringVar(&keyDir, "key-dir", "", "key directory for attestation check (default ~/.config/firedrill)")
	return cmd
}
