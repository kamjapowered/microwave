// Package microwave is the implementation of the microwave umbrella
// generator. The CLI entrypoint lives in microwave/cmd; this package
// holds the scan -> validate -> emit pipeline.
package microwave

import (
	"errors"
	"fmt"
	"go/token"
	"io"
	"os"
	"sort"
)

// Config carries the user-supplied invocation parameters into the
// pipeline. All fields are required except Stderr (defaults to
// os.Stderr).
type Config struct {
	// Paths is the set of scan targets passed on the command line.
	Paths []string

	// Out is the path of the generated Go file. Must resolve inside
	// the current Go module.
	Out string

	// Pkg is the package name declared at the top of the generated
	// file.
	Pkg string

	// Args is os.Args[1:] verbatim, recorded into the generated
	// //go:generate line so re-running reproduces the file.
	Args []string

	// Stderr receives diagnostic output. If nil, os.Stderr is used.
	Stderr io.Writer
}

// Sentinel errors returned from Run so the CLI can map them to the
// right exit codes. See spec §9.
var (
	ErrUsage    = errors.New("usage error")
	ErrPipeline = errors.New("pipeline error")
)

// Run executes the full scan -> validate -> emit pipeline.
func Run(cfg Config) error {
	stderr := cfg.Stderr
	if stderr == nil {
		stderr = os.Stderr
	}

	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(stderr, "microwave: cannot determine cwd: %v\n", err)
		return ErrUsage
	}
	modRoot, modGo, err := findModuleRoot(cwd)
	if err != nil {
		fmt.Fprintf(stderr, "microwave: %v\n", err)
		return ErrUsage
	}
	if _, err := validateOutPath(cfg.Out, modRoot); err != nil {
		fmt.Fprintf(stderr, "microwave: %v\n", err)
		return ErrUsage
	}

	res, err := scan(cfg, modRoot, modGo)
	if err != nil {
		fmt.Fprintf(stderr, "%v\n", err)
		return ErrPipeline
	}
	printDiags(stderr, res.Diags)
	if HasErrors(res.Diags) {
		return ErrPipeline
	}

	if len(res.Decls) == 0 {
		fmt.Fprintf(stderr, "microwave: error: no //microwave:export tags found in any scanned path: %v [%s]\n",
			cfg.Paths, RuleNoTags)
		return ErrPipeline
	}

	lookup, vdiags := validate(res.Decls, res.ModGo)
	printDiags(stderr, vdiags)
	if HasErrors(vdiags) {
		return ErrPipeline
	}

	ediags, err := emit(cfg, res.Decls, lookup)
	printDiags(stderr, ediags)
	if err != nil {
		fmt.Fprintf(stderr, "microwave: %v\n", err)
		return ErrPipeline
	}
	if HasErrors(ediags) {
		return ErrPipeline
	}
	return nil
}

// printDiags writes diagnostics to w in deterministic order.
func printDiags(w io.Writer, diags []Diagnostic) {
	sorted := make([]Diagnostic, len(diags))
	copy(sorted, diags)
	sort.SliceStable(sorted, func(i, j int) bool {
		a, b := sorted[i], sorted[j]
		if a.Pos.Filename != b.Pos.Filename {
			return a.Pos.Filename < b.Pos.Filename
		}
		if a.Pos.Line != b.Pos.Line {
			return a.Pos.Line < b.Pos.Line
		}
		if a.Pos.Column != b.Pos.Column {
			return a.Pos.Column < b.Pos.Column
		}
		return a.Rule < b.Rule
	})
	for _, d := range sorted {
		fmt.Fprintf(w, "microwave: %s: %s: %s [%s]\n",
			formatPos(d.Pos), d.Severity, d.Message, d.Rule)
	}
}

func formatPos(p token.Position) string {
	if p.Filename == "" {
		return "<unknown>"
	}
	if p.Line == 0 {
		return p.Filename
	}
	if p.Column == 0 {
		return fmt.Sprintf("%s:%d", p.Filename, p.Line)
	}
	return fmt.Sprintf("%s:%d:%d", p.Filename, p.Line, p.Column)
}
