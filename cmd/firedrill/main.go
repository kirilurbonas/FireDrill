// Command firedrill runs recovery drills: it restores real backups into
// disposable sandboxes, verifies the data, and emits signed evidence.
package main

import (
	"crypto/ed25519"
	"encoding/pem"
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
	root.AddCommand(runCmd(), validateCmd(), keygenCmd(), verifyEvidenceCmd(), operatorCmd())
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
	cmd := &cobra.Command{
		Use:   "run <drill-name>",
		Short: "Execute a recovery drill",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			d, err := spec.Load(file)
			if err != nil {
				return err
			}
			if d.Metadata.Name != args[0] {
				return fmt.Errorf("drill %q not found in %s (contains %q)", args[0], file, d.Metadata.Name)
			}
			p := &report.Printer{
				W:     os.Stdout,
				Color: !noColor && isatty.IsTerminal(os.Stdout.Fd()),
			}
			if dryRun {
				p.Info("dry run — would provision %s sandbox %s (ttl %s), restore %s, run %d checks",
					d.Spec.Sandbox.Provider, d.Spec.Sandbox.Image, d.Spec.Sandbox.TTL,
					d.Spec.Source.From.URI, len(d.Spec.Verify))
				return nil
			}
			e, path, err := drill.Run(cmd.Context(), d, drill.Options{
				Printer:     p,
				EvidenceDir: evidenceDir,
				KeyDir:      keyDir,
				Version:     version.Version,
			})
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
	cmd.Flags().StringVarP(&file, "file", "f", "firedrill.yaml", "drill spec file")
	cmd.Flags().StringVar(&evidenceDir, "evidence-dir", "", "override evidence output directory")
	cmd.Flags().StringVar(&keyDir, "key-dir", "", "signing key directory (default ~/.config/firedrill)")
	cmd.Flags().BoolVar(&noColor, "no-color", false, "disable colored output")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print the plan without touching Docker")
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
			d, err := spec.Load(file)
			if err != nil {
				return err
			}
			fmt.Printf("✓ %s is valid — drill %q, %d checks, sandbox %s (ttl %s)\n",
				file, d.Metadata.Name, len(d.Spec.Verify), d.Spec.Sandbox.Image, d.Spec.Sandbox.TTL)
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
	var pubPath string
	cmd := &cobra.Command{
		Use:   "verify-evidence <evidence.json>",
		Short: "Verify the signature on an evidence file",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var trusted ed25519.PublicKey
			if pubPath != "" {
				data, err := os.ReadFile(pubPath) // #nosec G304 -- user-supplied key path
				if err != nil {
					return err
				}
				block, _ := pem.Decode(data)
				if block == nil || len(block.Bytes) != ed25519.PublicKeySize {
					return fmt.Errorf("malformed public key %s", pubPath)
				}
				trusted = ed25519.PublicKey(block.Bytes)
			}
			if err := report.Verify(args[0], trusted); err != nil {
				return err
			}
			fmt.Println("✓ signature valid — evidence is intact")
			return nil
		},
	}
	cmd.Flags().StringVar(&pubPath, "public-key", "", "require signature from this public key (PEM)")
	return cmd
}
