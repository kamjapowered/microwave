package microwave

import (
	"fmt"
	"go/ast"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
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
func scan(cfg Config, modRoot, modGo string) (scanResult, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return scanResult{}, fmt.Errorf("microwave: cannot determine cwd: %w", err)
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

	pkgs, err := packages.Load(pcfg, cfg.Paths...)
	if err != nil {
		return scanResult{}, fmt.Errorf("microwave: packages.Load: %w", err)
	}

	res := scanResult{ModRoot: modRoot, ModGo: modGo}

	// Fail fast on any per-package load errors.
	for _, pkg := range pkgs {
		for _, perr := range pkg.Errors {
			res.Diags = append(res.Diags, Diagnostic{
				Severity: SevError,
				Pos:      loadErrPos(perr),
				Rule:     RuleLoadError,
				Message:  perr.Msg,
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

// findModuleRoot walks up from start looking for a go.mod, returning
// the directory containing it and the value of the `go` directive.
func findModuleRoot(start string) (root, goVersion string, err error) {
	dir := start
	for {
		gomod := filepath.Join(dir, "go.mod")
		if data, rerr := os.ReadFile(gomod); rerr == nil {
			v, perr := parseGoDirective(data)
			if perr != nil {
				return "", "", perr
			}
			return dir, v, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", "", fmt.Errorf("no go.mod found in %s or any parent", start)
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
				Message:  "microwave:export tag is not contiguous with a declaration; tag ignored",
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
			Message:  "malformed microwave:export tag (expected `//microwave:export` or `//microwave:export NewName`)",
		}}, false
	}
	if multi {
		return TaggedDecl{}, []Diagnostic{{
			Severity: SevError,
			Pos:      pkg.Fset.Position(fd.Doc.Pos()),
			Rule:     RuleMultiTag,
			Message:  "more than one //microwave:export tag in a single doc comment group; one tag per declaration",
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
			Message:  fmt.Sprintf("cannot tag method %s; tag its receiver type instead", fd.Name.Name),
		}}, false
	}

	name := fd.Name.Name
	if name == "_" {
		return TaggedDecl{}, []Diagnostic{{
			Severity: SevError,
			Pos:      pkg.Fset.Position(fd.Pos()),
			Rule:     RuleBlankDecl,
			Message:  "cannot tag a blank (`_`) declaration",
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
			Message:  "malformed microwave:export tag (expected `//microwave:export` or `//microwave:export NewName`)",
		})
		gdFound = false
	}
	if gdMulti {
		diags = append(diags, Diagnostic{
			Severity: SevError,
			Pos:      pkg.Fset.Position(gd.Doc.Pos()),
			Rule:     RuleMultiTag,
			Message:  "more than one //microwave:export tag in a single doc comment group; one tag per declaration",
		})
		gdFound = false
	}

	if gdFound && multiSpec {
		diags = append(diags, Diagnostic{
			Severity: SevError,
			Pos:      pkg.Fset.Position(gd.Doc.Pos()),
			Rule:     RuleTagOnGroup,
			Message:  "microwave:export tag sits above a multi-spec group; tag individual decls inside the group, not the group itself",
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
			Message:  "malformed microwave:export tag",
		}}, false
	}
	if specMulti {
		return TaggedDecl{}, []Diagnostic{{
			Severity: SevError,
			Pos:      pkg.Fset.Position(s.Doc.Pos()),
			Rule:     RuleMultiTag,
			Message:  "more than one //microwave:export tag in a single doc comment group; one tag per declaration",
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
			Message:  "cannot tag a blank (`_`) type",
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
			Message:  "malformed microwave:export tag",
		}}, false
	}
	if specMulti {
		return TaggedDecl{}, []Diagnostic{{
			Severity: SevError,
			Pos:      pkg.Fset.Position(s.Doc.Pos()),
			Rule:     RuleMultiTag,
			Message:  "more than one //microwave:export tag in a single doc comment group; one tag per declaration",
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
			Message:  "microwave:export cannot tag a spec that declares more than one name; split it into separate specs",
		}}, false
	}

	name := s.Names[0].Name
	if name == "_" {
		return TaggedDecl{}, []Diagnostic{{
			Severity: SevError,
			Pos:      pkg.Fset.Position(s.Pos()),
			Rule:     RuleBlankDecl,
			Message:  "cannot tag a blank (`_`) declaration",
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
