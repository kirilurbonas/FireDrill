package report

import (
	"fmt"
	"io"
	"strings"
	"time"
)

const (
	green  = "\033[32m"
	red    = "\033[31m"
	yellow = "\033[33m"
	dim    = "\033[2m"
	bold   = "\033[1m"
	reset  = "\033[0m"
)

// Printer renders drill progress in the style of the README demo.
type Printer struct {
	W     io.Writer
	Color bool
}

func (p *Printer) c(code, s string) string {
	if !p.Color {
		return s
	}
	return code + s + reset
}

// Step prints a left-aligned action line padded with dots and its outcome.
func (p *Printer) Step(action, outcome string, ok bool) {
	pad := 52 - len(action)
	if pad < 1 {
		pad = 1
	}
	status := p.c(green, outcome)
	if !ok {
		status = p.c(red, outcome)
	}
	fmt.Fprintf(p.W, "▸ %s %s %s\n", action, p.c(dim, strings.Repeat(".", pad)), status)
}

func (p *Printer) Info(format string, a ...any) {
	fmt.Fprintf(p.W, "▸ "+format+"\n", a...)
}

// Summary prints the final verdict block.
func (p *Printer) Summary(e *Evidence, evidencePath string, signed bool) {
	rto := time.Duration(e.Measured.RestoreSeconds * float64(time.Second)).Round(time.Second)
	age := time.Duration(e.Backup.AgeSecs * float64(time.Second)).Round(time.Minute)
	mark := func(ok bool) string {
		if ok {
			return p.c(green, "✓")
		}
		return p.c(red, "✗")
	}
	fmt.Fprintf(p.W, "▸ measured RTO %s (target %s %s)   RPO %s (target %s %s)\n",
		rto, e.Objectives.RTO, mark(e.Measured.RTOMet), age, e.Objectives.RPO, mark(e.Measured.RPOMet))
	sig := ""
	if signed {
		sig = "  " + p.c(dim, "(signed ✓)")
	}
	fmt.Fprintf(p.W, "▸ evidence %s%s\n", evidencePath, sig)
	if e.Verified {
		fmt.Fprintf(p.W, "%s\n", p.c(bold+green, "✔ RECOVERY VERIFIED — sandbox destroyed"))
	} else {
		fmt.Fprintf(p.W, "%s\n", p.c(bold+red, "✘ RECOVERY FAILED — see evidence for details"))
	}
}
