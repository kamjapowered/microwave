package microwave

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"golang.org/x/tools/go/packages"
)

// scanResult bundles the outputs of the scan phase.
type scanResult struct {
	Decls   []TaggedDecl
	Diags   []Diagnostic
	ModRoot string // absolute path to module root
	ModGo   string // value of the `go` directive in go.mod (e.g. "1.24")
}

// scan runs the scan phase: discover the module, load packages, walk
// ASTs and parse tags. Floating-tag warnings are produced here.
//
// Returns a non-nil error only for "cannot continue" conditions
// (cwd/module discovery failure). Validation/load errors are surfaced
// as diagnostics in the result.
func scan(cfg Config, modRoot, modGo, facadePath string) (scanResult, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return scanResult{}, fmt.Errorf("microwave: cannot determine cwd: %w", err)
	}

	resolved, rdiags, err := resolveScanSet(cwd, cfg.Paths, cfg.Excludes, facadePath)
	if err != nil {
		return scanResult{}, err
	}

	res := scanResult{ModRoot: modRoot, ModGo: modGo, Diags: rdiags}
	if len(resolved) == 0 {
		// Every scan path was excluded (or matched no packages). No
		// type-check needed; downstream "no tags found" will fire.
		return res, nil
	}

	pcfg := &packages.Config{
		Mode: packages.NeedName |
			packages.NeedFiles |
			packages.NeedSyntax |
			packages.NeedTypes |
			packages.NeedTypesInfo |
			packages.NeedImports |
			packages.NeedDeps,
		Dir:  cwd,
		Fset: token.NewFileSet(),
	}

	pkgs, err := packages.Load(pcfg, resolved...)
	if err != nil {
		return scanResult{}, fmt.Errorf("microwave: packages.Load: %w", err)
	}

	// Fail fast on any per-package load errors.
	for _, pkg := range pkgs {
		for _, perr := range pkg.Errors {
			res.Diags = append(res.Diags, Diagnostic{
				Severity: SevError,
				Pos:      loadErrPos(perr),
				Rule:     RuleLoadError,
				Summary:  "package load failed",
				Fields:   []Field{F("error", perr.Msg)},
			})
		}
	}
	if HasErrors(res.Diags) {
		return res, nil
	}

	for _, pkg := range pkgs {
		if isInternalPath(pkg.PkgPath) {
			continue
		}
		for _, file := range pkg.Syntax {
			fname := pkg.Fset.Position(file.Pos()).Filename
			if strings.HasSuffix(fname, "_test.go") {
				continue
			}
			pd, fd := walkFile(file, pkg)
			res.Decls = append(res.Decls, pd...)
			res.Diags = append(res.Diags, fd...)
		}
	}

	return res, nil
}

// resolveScanSet expands the user's positional scan paths and
// --exclude patterns into a concrete list of package import paths to
// type-check. It uses a fast metadata-only load (no types, no AST) so
// the heavy compile pass only sees the producer packages.
//
// Excluded packages are subtracted by exact import path match after
// expansion, so `--exclude ./pkg/consumer` and
// `--exclude ./pkg/consumer/...` both work — but only if the named
// path actually resolves to one or more packages. An exclude that
// matches nothing (typically a parent directory with sub-packages but
// no direct package, like `./backends`) emits an exclude-no-match
// warning suggesting the recursive form.
//
// Packages that import facadePath — the umbrella package about to be
// regenerated — are dropped automatically: a re-exporter can never
// scan a package that consumes its own output (the build is circular,
// and during regeneration the umbrella is removed, so they would not
// type-check anyway). This means opt-in plugins built against the
// umbrella self-exclude without an --exclude entry. facadePath is the
// umbrella's own path, so the umbrella package never scans itself.
func resolveScanSet(cwd string, paths, excludes []string, facadePath string) ([]string, []Diagnostic, error) {
	if len(paths) == 0 {
		return nil, nil, nil
	}
	pcfg := &packages.Config{
		Mode: packages.NeedName | packages.NeedFiles,
		Dir:  cwd,
	}

	included, err := packages.Load(pcfg, paths...)
	if err != nil {
		return nil, nil, fmt.Errorf("microwave: resolve scan paths: %w", err)
	}

	var diags []Diagnostic
	excluded := map[string]bool{}
	for _, e := range excludes {
		exPkgs, err := packages.Load(pcfg, e)
		if err != nil {
			return nil, nil, fmt.Errorf("microwave: resolve --exclude %q: %w", e, err)
		}
		matched := false
		for _, p := range exPkgs {
			if p.PkgPath == "" {
				continue
			}
			// Skip synthetic packages with no real source files (e.g.
			// command-line-arguments returned when the pattern names a
			// directory that has no direct Go files).
			if len(p.GoFiles) == 0 {
				continue
			}
			excluded[p.PkgPath] = true
			matched = true
		}
		if !matched {
			diags = append(diags, Diagnostic{
				Severity: SevWarning,
				Rule:     RuleExcludeNoMatch,
				Summary:  "no packages matched --exclude",
				Fields: []Field{
					F("pattern", e),
					F("hint", "for a directory of sub-packages use "+strings.TrimRight(e, "/")+"/..."),
				},
			})
		}
	}

	seen := map[string]bool{}
	out := make([]string, 0, len(included))
	for _, p := range included {
		if p.PkgPath == "" || seen[p.PkgPath] || excluded[p.PkgPath] {
			continue
		}
		if p.PkgPath == facadePath || importsFacade(p.GoFiles, facadePath) {
			continue
		}
		seen[p.PkgPath] = true
		out = append(out, p.PkgPath)
	}
	return out, diags, nil
}

// importsFacade reports whether any of goFiles imports facadePath. It
// parses imports only (no type-checking), so it is robust even while
// the umbrella package is absent during regeneration.
func importsFacade(goFiles []string, facadePath string) bool {
	if facadePath == "" {
		return false
	}
	fset := token.NewFileSet()
	for _, f := range goFiles {
		af, err := parser.ParseFile(fset, f, nil, parser.ImportsOnly)
		if err != nil {
			continue
		}
		for _, imp := range af.Imports {
			path, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				continue
			}
			if path == facadePath {
				return true
			}
		}
	}
	return false
}

// findModuleRoot walks up from start looking for a go.mod, returning
// the directory containing it, the value of the `go` directive, and the
// module path from the `module` directive.
func findModuleRoot(start string) (root, goVersion, modPath string, err error) {
	dir := start
	for {
		gomod := filepath.Join(dir, "go.mod")
		if data, rerr := os.ReadFile(gomod); rerr == nil {
			v, perr := parseGoDirective(data)
			if perr != nil {
				return "", "", "", perr
			}
			mp, perr := parseModulePath(data)
			if perr != nil {
				return "", "", "", perr
			}
			return dir, v, mp, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", "", "", fmt.Errorf("no go.mod found in %s or any parent", start)
		}
		dir = parent
	}
}

var goDirectiveRE = regexp.MustCompile(`(?m)^[ \t]*go[ \t]+([0-9]+\.[0-9]+(?:\.[0-9]+)?)`)

func parseGoDirective(data []byte) (string, error) {
	m := goDirectiveRE.FindSubmatch(data)
	if m == nil {
		return "", fmt.Errorf("go.mod missing or unparseable `go` directive")
	}
	return string(m[1]), nil
}

var modulePathRE = regexp.MustCompile(`(?m)^[ \t]*module[ \t]+(\S+)`)

func parseModulePath(data []byte) (string, error) {
	m := modulePathRE.FindSubmatch(data)
	if m == nil {
		return "", fmt.Errorf("go.mod missing or unparseable `module` directive")
	}
	return string(m[1]), nil
}

// facadeImportPath returns the import path of the umbrella package being
// generated: the module path joined with the out file's directory
// relative to the module root. For an out file at the module root this
// is the module path itself.
func facadeImportPath(modPath, modRoot, absOut string) string {
	rel, err := filepath.Rel(modRoot, filepath.Dir(absOut))
	if err != nil || rel == "." {
		return modPath
	}
	return modPath + "/" + filepath.ToSlash(rel)
}

// validateOutPath ensures --out resolves inside modRoot. Returns the
// cleaned absolute path on success.
func validateOutPath(out, modRoot string) (string, error) {
	abs, err := filepath.Abs(out)
	if err != nil {
		return "", fmt.Errorf("cannot resolve --out path %q: %w", out, err)
	}
	abs = filepath.Clean(abs)
	rel, err := filepath.Rel(modRoot, abs)
	if err != nil {
		return "", fmt.Errorf("cannot resolve --out relative to module root: %w", err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("--out path %q resolves outside the module root %q", abs, modRoot)
	}
	return abs, nil
}

// isInternalPath reports whether any path segment is exactly "internal",
// matching Go's own internal-package rule (spec §6).
func isInternalPath(p string) bool {
	for _, seg := range strings.Split(p, "/") {
		if seg == "internal" {
			return true
		}
	}
	return false
}

func loadErrPos(e packages.Error) token.Position {
	// e.Pos is "file:line:col"; parse loosely.
	pos := token.Position{}
	parts := strings.Split(e.Pos, ":")
	if len(parts) >= 1 {
		pos.Filename = parts[0]
	}
	if len(parts) >= 2 {
		fmt.Sscanf(parts[1], "%d", &pos.Line)
	}
	if len(parts) >= 3 {
		fmt.Sscanf(parts[2], "%d", &pos.Column)
	}
	return pos
}

// tagPattern matches a single tag line. Group 1 is the rename token, if any.
// Allows an optional rename token, then optional trailing whitespace, then
// end of line. Any other trailing content makes the line malformed.
var tagPattern = regexp.MustCompile(`^//microwave:export(?:[ \t]+(\S+))?[ \t]*$`)

// hasTagAnywhere reports whether any line in cg begins with
// `//microwave:export`. Used for floating-tag detection.
func hasTagAnywhere(cg *ast.CommentGroup) bool {
	if cg == nil {
		return false
	}
	for _, c := range cg.List {
		if strings.HasPrefix(c.Text, "//microwave:export") {
			return true
		}
	}
	return false
}

// extractTag scans every line of cg for a `//microwave:export` tag.
//
// The tag may appear at any position in the comment group. gofmt
// detects `//microwave:export` as a directive comment (`//word:rest`,
// no space after `//`) and normalises it to sit immediately above the
// declaration, beneath any human-readable doc comment and after an
// inserted blank `//` separator. extractTag accepts both the original
// "tag-first" form and gofmt's normalised form.
//
// Returns:
//   - rename: the rename token, or "" if absent
//   - doc: the remaining doc-comment lines joined with "\n", with the
//     blank `//` separator that gofmt inserts adjacent to the tag
//     stripped
//   - found: true if exactly one well-formed tag line is present
//   - malformed: true if some line begins with `//microwave:export`
//     but does not match the tag grammar
//   - multiple: true if more than one well-formed tag line is present
func extractTag(cg *ast.CommentGroup) (rename string, doc string, found bool, malformed bool, multiple bool) {
	if cg == nil {
		return "", "", false, false, false
	}

	tagIdx := -1
	for i, c := range cg.List {
		if !strings.HasPrefix(c.Text, "//microwave:export") {
			continue
		}
		m := tagPattern.FindStringSubmatch(c.Text)
		if m == nil {
			return "", "", false, true, false
		}
		if tagIdx != -1 {
			return "", "", false, false, true
		}
		tagIdx = i
		rename = m[1]
	}
	if tagIdx == -1 {
		return "", "", false, false, false
	}

	var lines []string
	for i, c := range cg.List {
		if i == tagIdx {
			continue
		}
		if (i == tagIdx-1 || i == tagIdx+1) && isBlankComment(c.Text) {
			continue
		}
		lines = append(lines, c.Text)
	}
	doc = strings.Join(lines, "\n")
	return rename, doc, true, false, false
}

// isBlankComment reports whether t is a `//` line with no content,
// such as the separator gofmt inserts between a doc comment and a
// trailing directive.
func isBlankComment(t string) bool {
	rest := strings.TrimPrefix(t, "//")
	return strings.TrimSpace(rest) == ""
}

// walkFile collects TaggedDecls and diagnostics from a single source file.
func walkFile(file *ast.File, pkg *packages.Package) ([]TaggedDecl, []Diagnostic) {
	var decls []TaggedDecl
	var diags []Diagnostic

	// Track which CommentGroups we've already inspected as a Doc field
	// on some node — needed to detect floating tags later.
	attached := make(map[*ast.CommentGroup]struct{})
	mark := func(cg *ast.CommentGroup) {
		if cg != nil {
			attached[cg] = struct{}{}
		}
	}

	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			mark(d.Doc)
			td, ds, ok := processFuncDecl(d, pkg)
			diags = append(diags, ds...)
			if ok {
				decls = append(decls, td)
			}
		case *ast.GenDecl:
			mark(d.Doc)
			for _, sp := range d.Specs {
				switch s := sp.(type) {
				case *ast.TypeSpec:
					mark(s.Doc)
				case *ast.ValueSpec:
					mark(s.Doc)
				}
			}
			td, ds := processGenDecl(d, pkg)
			diags = append(diags, ds...)
			decls = append(decls, td...)
		}
	}

	for _, cg := range file.Comments {
		if _, ok := attached[cg]; ok {
			continue // attached groups are processed via extractTag above
		}
		for _, c := range cg.List {
			if !strings.HasPrefix(c.Text, "//microwave:export") {
				continue
			}
			diags = append(diags, Diagnostic{
				Severity: SevWarning,
				Pos:      pkg.Fset.Position(c.Pos()),
				Rule:     RuleFloatingTag,
				Summary:  "tag not contiguous with a declaration; ignored",
			})
		}
	}

	return decls, diags
}

func processFuncDecl(fd *ast.FuncDecl, pkg *packages.Package) (TaggedDecl, []Diagnostic, bool) {
	rename, doc, found, malformed, multi := extractTag(fd.Doc)
	if malformed {
		return TaggedDecl{}, []Diagnostic{{
			Severity: SevError,
			Pos:      pkg.Fset.Position(fd.Doc.Pos()),
			Rule:     RuleTagMalformed,
			Summary:  "malformed //microwave:export tag",
			Fields:   []Field{F("expected", "//microwave:export or //microwave:export NewName")},
		}}, false
	}
	if multi {
		return TaggedDecl{}, []Diagnostic{{
			Severity: SevError,
			Pos:      pkg.Fset.Position(fd.Doc.Pos()),
			Rule:     RuleMultiTag,
			Summary:  "multiple //microwave:export tags in one comment group",
			Fields:   []Field{F("hint", "each declaration may carry at most one tag")},
		}}, false
	}
	if !found {
		return TaggedDecl{}, nil, false
	}

	if fd.Recv != nil {
		return TaggedDecl{}, []Diagnostic{{
			Severity: SevError,
			Pos:      pkg.Fset.Position(fd.Pos()),
			Rule:     RuleMethodTag,
			Summary:  "methods cannot be tagged",
			Fields: []Field{
				F("method", fd.Name.Name),
				F("fix", "tag the receiver type instead"),
			},
		}}, false
	}

	name := fd.Name.Name
	if name == "_" {
		return TaggedDecl{}, []Diagnostic{{
			Severity: SevError,
			Pos:      pkg.Fset.Position(fd.Pos()),
			Rule:     RuleBlankDecl,
			Summary:  "blank (_) declarations cannot be tagged",
		}}, false
	}

	emit := name
	if rename != "" {
		emit = rename
	}

	return TaggedDecl{
		Kind:       KindFunc,
		SourcePkg:  pkg.PkgPath,
		SourceName: name,
		EmitName:   emit,
		DocComment: doc,
		Pos:        pkg.Fset.Position(fd.Pos()),
		Decl:       fd,
		Obj:        pkg.TypesInfo.Defs[fd.Name],
		Pkg:        pkg,
	}, nil, true
}

func processGenDecl(gd *ast.GenDecl, pkg *packages.Package) ([]TaggedDecl, []Diagnostic) {
	if gd.Tok == token.IMPORT {
		return nil, nil
	}

	var decls []TaggedDecl
	var diags []Diagnostic

	multiSpec := len(gd.Specs) > 1

	gdRename, gdDoc, gdFound, gdMalformed, gdMulti := extractTag(gd.Doc)
	if gdMalformed {
		diags = append(diags, Diagnostic{
			Severity: SevError,
			Pos:      pkg.Fset.Position(gd.Doc.Pos()),
			Rule:     RuleTagMalformed,
			Summary:  "malformed //microwave:export tag",
			Fields:   []Field{F("expected", "//microwave:export or //microwave:export NewName")},
		})
		gdFound = false
	}
	if gdMulti {
		diags = append(diags, Diagnostic{
			Severity: SevError,
			Pos:      pkg.Fset.Position(gd.Doc.Pos()),
			Rule:     RuleMultiTag,
			Summary:  "multiple //microwave:export tags in one comment group",
			Fields:   []Field{F("hint", "each declaration may carry at most one tag")},
		})
		gdFound = false
	}

	if gdFound && multiSpec {
		diags = append(diags, Diagnostic{
			Severity: SevError,
			Pos:      pkg.Fset.Position(gd.Doc.Pos()),
			Rule:     RuleTagOnGroup,
			Summary:  "tag sits above a multi-spec group",
			Fields:   []Field{F("fix", "tag individual specs inside the group instead")},
		})
		gdFound = false
	}

	for i, sp := range gd.Specs {
		switch s := sp.(type) {
		case *ast.TypeSpec:
			td, ds, ok := processTypeSpec(s, pkg, gdRename, gdDoc, gdFound && i == 0 && !multiSpec)
			diags = append(diags, ds...)
			if ok {
				decls = append(decls, td)
			}
		case *ast.ValueSpec:
			td, ds, ok := processValueSpec(s, gd, pkg, gdRename, gdDoc, gdFound && i == 0 && !multiSpec)
			diags = append(diags, ds...)
			if ok {
				decls = append(decls, td)
			}
		}
	}

	return decls, diags
}

func processTypeSpec(s *ast.TypeSpec, pkg *packages.Package, gdRename, gdDoc string, applyOuter bool) (TaggedDecl, []Diagnostic, bool) {
	specRename, specDoc, specFound, specMalformed, specMulti := extractTag(s.Doc)
	if specMalformed {
		return TaggedDecl{}, []Diagnostic{{
			Severity: SevError,
			Pos:      pkg.Fset.Position(s.Doc.Pos()),
			Rule:     RuleTagMalformed,
			Summary:  "malformed //microwave:export tag",
			Fields:   []Field{F("expected", "//microwave:export or //microwave:export NewName")},
		}}, false
	}
	if specMulti {
		return TaggedDecl{}, []Diagnostic{{
			Severity: SevError,
			Pos:      pkg.Fset.Position(s.Doc.Pos()),
			Rule:     RuleMultiTag,
			Summary:  "multiple //microwave:export tags in one comment group",
			Fields:   []Field{F("hint", "each declaration may carry at most one tag")},
		}}, false
	}

	var rename, doc string
	switch {
	case specFound:
		rename, doc = specRename, specDoc
	case applyOuter:
		rename, doc = gdRename, gdDoc
	default:
		return TaggedDecl{}, nil, false
	}

	name := s.Name.Name
	if name == "_" {
		return TaggedDecl{}, []Diagnostic{{
			Severity: SevError,
			Pos:      pkg.Fset.Position(s.Pos()),
			Rule:     RuleBlankDecl,
			Summary:  "blank (_) declarations cannot be tagged",
		}}, false
	}

	emit := name
	if rename != "" {
		emit = rename
	}

	return TaggedDecl{
		Kind:       KindType,
		SourcePkg:  pkg.PkgPath,
		SourceName: name,
		EmitName:   emit,
		DocComment: doc,
		Pos:        pkg.Fset.Position(s.Pos()),
		Decl:       s,
		Obj:        pkg.TypesInfo.Defs[s.Name],
		Pkg:        pkg,
	}, nil, true
}

func processValueSpec(s *ast.ValueSpec, gd *ast.GenDecl, pkg *packages.Package, gdRename, gdDoc string, applyOuter bool) (TaggedDecl, []Diagnostic, bool) {
	specRename, specDoc, specFound, specMalformed, specMulti := extractTag(s.Doc)
	if specMalformed {
		return TaggedDecl{}, []Diagnostic{{
			Severity: SevError,
			Pos:      pkg.Fset.Position(s.Doc.Pos()),
			Rule:     RuleTagMalformed,
			Summary:  "malformed //microwave:export tag",
			Fields:   []Field{F("expected", "//microwave:export or //microwave:export NewName")},
		}}, false
	}
	if specMulti {
		return TaggedDecl{}, []Diagnostic{{
			Severity: SevError,
			Pos:      pkg.Fset.Position(s.Doc.Pos()),
			Rule:     RuleMultiTag,
			Summary:  "multiple //microwave:export tags in one comment group",
			Fields:   []Field{F("hint", "each declaration may carry at most one tag")},
		}}, false
	}

	var rename, doc string
	switch {
	case specFound:
		rename, doc = specRename, specDoc
	case applyOuter:
		rename, doc = gdRename, gdDoc
	default:
		return TaggedDecl{}, nil, false
	}

	if len(s.Names) != 1 {
		return TaggedDecl{}, []Diagnostic{{
			Severity: SevError,
			Pos:      pkg.Fset.Position(s.Pos()),
			Rule:     RuleMultiNameSpec,
			Summary:  "tagged spec declares more than one name",
			Fields:   []Field{F("fix", "split into separate specs, one tag each")},
		}}, false
	}

	name := s.Names[0].Name
	if name == "_" {
		return TaggedDecl{}, []Diagnostic{{
			Severity: SevError,
			Pos:      pkg.Fset.Position(s.Pos()),
			Rule:     RuleBlankDecl,
			Summary:  "blank (_) declarations cannot be tagged",
		}}, false
	}

	emit := name
	if rename != "" {
		emit = rename
	}

	kind := KindVar
	if gd.Tok == token.CONST {
		kind = KindConst
	}

	return TaggedDecl{
		Kind:       kind,
		SourcePkg:  pkg.PkgPath,
		SourceName: name,
		EmitName:   emit,
		DocComment: doc,
		Pos:        pkg.Fset.Position(s.Pos()),
		Decl:       s,
		Obj:        pkg.TypesInfo.Defs[s.Names[0]],
		Pkg:        pkg,
	}, nil, true
}
