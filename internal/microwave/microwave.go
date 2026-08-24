// Package microwave is the implementation of the microwave umbrella
// generator. The CLI entrypoint lives in the cmd package; this package
// holds the scan -> validate -> emit pipeline.
package microwave

import (
	"errors"
	"fmt"
	"go/token"
	"io"
	"io/fs"
	"os"
	"sort"
)

// Config carries the user-supplied invocation parameters into the
// pipeline. All fields are required except Stderr (defaults to
// os.Stderr).
type Config struct {
	// Paths is the set of scan targets passed on the command line.
	Paths []string

	// Excludes are package patterns to subtract from the resolved scan
	// set. Accepts the same syntax as Paths (relative dirs, ./...
	// recursive patterns, import paths). Useful when a wider scan
	// (e.g. ./pkg/...) sweeps in consumer packages whose build
	// depends on the umbrella that is about to be regenerated.
	Excludes []string

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
		PrintError(stderr, fmt.Sprintf("cannot determine cwd: %v", err))
		return ErrUsage
	}
	modRoot, modGo, modPath, err := findModuleRoot(cwd)
	if err != nil {
		PrintError(stderr, err.Error())
		return ErrUsage
	}
	absOut, err := validateOutPath(cfg.Out, modRoot)
	if err != nil {
		PrintError(stderr, err.Error())
		return ErrUsage
	}
	facadePath := facadeImportPath(modPath, modRoot, absOut)

	// Stash the existing umbrella file (if any) and remove it before
	// loading packages. If we left a stale copy in place, its
	// references to renamed/removed source symbols would surface as
	// load errors and block the very regeneration that would fix
	// them. On any pipeline failure below, the backup is restored so
	// the user never ends up with no file.
	backup, hadOut, sterr := stashOutput(absOut)
	if sterr != nil {
		PrintError(stderr, sterr.Error())
		return ErrPipeline
	}
	succeeded := false
	defer func() {
		if !succeeded && hadOut {
			_ = os.WriteFile(absOut, backup, 0o644)
		}
	}()

	res, err := scan(cfg, modRoot, modGo, facadePath)
	if err != nil {
		PrintError(stderr, err.Error())
		return ErrPipeline
	}
	printDiags(stderr, res.Diags)
	if HasErrors(res.Diags) {
		return ErrPipeline
	}

	if len(res.Decls) == 0 {
		printDiag(stderr, newStyles(stderr), Diagnostic{
			Severity: SevError,
			Rule:     RuleNoTags,
			Summary:  "no //microwave:export tags found",
			Fields:   []Field{F("scan paths", fmt.Sprintf("%v", cfg.Paths))},
		})
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
		PrintError(stderr, err.Error())
		return ErrPipeline
	}
	if HasErrors(ediags) {
		return ErrPipeline
	}
	succeeded = true

	warnings := len(res.Diags) + len(vdiags) + len(ediags)
	printSummary(stderr, cfg, res.Decls, warnings)
	return nil
}

// printSummary writes the success box: output path, package, count per
// kind (omitting zeros), source-package count, and warning count if
// any.
func printSummary(w io.Writer, cfg Config, decls []TaggedDecl, warnings int) {
	counts := map[Kind]int{}
	pkgs := map[string]bool{}
	for _, d := range decls {
		counts[d.Kind]++
		pkgs[d.SourcePkg] = true
	}

	row := func(label string, n int) Row {
		if n == 0 {
			return Row{}
		}
		return Row{Label: label, Value: fmt.Sprintf("%d", n)}
	}

	PrintBox(w, "generated", []Row{
		{Label: "out", Value: cfg.Out},
		{Label: "pkg", Value: cfg.Pkg},
		row("types", counts[KindType]),
		row("vars", counts[KindVar]),
		row("consts", counts[KindConst]),
		row("funcs", counts[KindFunc]),
		row("packages", len(pkgs)),
		row("warnings", warnings),
	})
}

// stashOutput reads and removes the existing umbrella file so it
// cannot poison the type-checker with stale references during scan.
// Returns the original bytes and whether a file was present; the
// caller restores them on pipeline failure.
func stashOutput(path string) (data []byte, existed bool, err error) {
	data, err = os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("read existing --out %q: %w", path, err)
	}
	if rerr := os.Remove(path); rerr != nil && !errors.Is(rerr, fs.ErrNotExist) {
		return nil, false, fmt.Errorf("remove existing --out %q: %w", path, rerr)
	}
	return data, true, nil
}

// printDiags writes diagnostics to w in deterministic order, with one
// blank line between successive entries for readability.
func printDiags(w io.Writer, diags []Diagnostic) {
	if len(diags) == 0 {
		return
	}
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
	s := newStyles(w)
	for _, d := range sorted {
		fmt.Fprintln(w)
		printDiag(w, s, d)
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
