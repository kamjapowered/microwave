package microwave

import (
	"fmt"
	"go/ast"
	"go/types"
	"strings"
	"unicode"
)

// validate enforces the rules in spec §5. It returns the umbrella
// lookup map (source-qualified type name -> umbrella emit name) and
// any diagnostics produced. Errors halt the pipeline; warnings flow
// through to emit.
func validate(decls []TaggedDecl, modGo string) (map[string]string, []Diagnostic) {
	var diags []Diagnostic

	lookup := buildLookup(decls)

	emitSeen := make(map[string]TaggedDecl, len(decls))
	for _, d := range decls {
		if first, ok := emitSeen[d.EmitName]; ok {
			diags = append(diags, Diagnostic{
				Severity: SevError,
				Pos:      d.Pos,
				Rule:     RuleCollision,
				Message: fmt.Sprintf(
					"emit name %q collides with %s.%s at %s; resolve via a rename token",
					d.EmitName, first.SourcePkg, first.SourceName, first.Pos,
				),
			})
			continue
		}
		emitSeen[d.EmitName] = d
	}

	for _, d := range decls {
		diags = append(diags, validateDecl(d, modGo)...)
	}

	return lookup, diags
}

// buildLookup constructs the source-qualified -> emit-name map for
// type-kind decls. Used by emit's signature rewriter.
func buildLookup(decls []TaggedDecl) map[string]string {
	m := make(map[string]string, len(decls))
	for _, d := range decls {
		if d.Kind != KindType {
			continue
		}
		m[d.SourcePkg+"."+d.SourceName] = d.EmitName
	}
	return m
}

// validateDecl runs every applicable rule against a single tagged decl.
func validateDecl(d TaggedDecl, modGo string) []Diagnostic {
	var diags []Diagnostic

	if !isExportedIdent(d.SourceName) {
		diags = append(diags, Diagnostic{
			Severity: SevError,
			Pos:      d.Pos,
			Rule:     RuleUnexportedDecl,
			Message: fmt.Sprintf(
				"cannot re-export unexported %s %s.%s; Go's visibility rules make the source identifier unreachable",
				d.Kind, d.SourcePkg, d.SourceName,
			),
		})
	}
	if d.EmitName != d.SourceName && !isExportedIdent(d.EmitName) {
		diags = append(diags, Diagnostic{
			Severity: SevError,
			Pos:      d.Pos,
			Rule:     RuleLowercaseRen,
			Message: fmt.Sprintf(
				"rename token %q starts with a lowercase letter; the umbrella name must be exported",
				d.EmitName,
			),
		})
	}

	switch d.Kind {
	case KindType:
		diags = append(diags, validateType(d, modGo)...)
	case KindVar, KindConst:
		diags = append(diags, validateValue(d)...)
	case KindFunc:
		diags = append(diags, validateFunc(d)...)
	}

	return diags
}

func validateType(d TaggedDecl, modGo string) []Diagnostic {
	var diags []Diagnostic

	tn, ok := d.Obj.(*types.TypeName)
	if !ok || tn.Type() == nil {
		return diags
	}

	ts, _ := d.Decl.(*ast.TypeSpec)
	hasTypeParams := ts != nil && ts.TypeParams != nil && len(ts.TypeParams.List) > 0

	if hasTypeParams && goVersionLT(modGo, "1.24") {
		diags = append(diags, Diagnostic{
			Severity: SevError,
			Pos:      d.Pos,
			Rule:     RuleGenericPre124,
			Message: fmt.Sprintf(
				"generic type %s.%s requires module Go directive >= 1.24 (got %s) for generic type aliases",
				d.SourcePkg, d.SourceName, modGo,
			),
		})
	}

	if hasTypeParams {
		named, _ := tn.Type().(*types.Named)
		if named != nil {
			for i := 0; i < named.TypeParams().Len(); i++ {
				tp := named.TypeParams().At(i)
				for _, ref := range findUnexportedRefs(tp.Constraint(), nil) {
					diags = append(diags, Diagnostic{
						Severity: SevWarning,
						Pos:      d.Pos,
						Rule:     RuleUnexpConstraint,
						Message: fmt.Sprintf(
							"generic constraint of %s.%s references unexported type %s",
							d.SourcePkg, d.SourceName, refQualified(ref),
						),
					})
				}
			}
		}
	}

	if st, ok := tn.Type().Underlying().(*types.Struct); ok {
		seen := map[*types.TypeName]bool{}
		for i := 0; i < st.NumFields(); i++ {
			f := st.Field(i)
			if !f.Exported() {
				continue
			}
			for _, ref := range findUnexportedRefs(f.Type(), seen) {
				diags = append(diags, Diagnostic{
					Severity: SevWarning,
					Pos:      d.Pos,
					Rule:     RuleUnexpFieldType,
					Message: fmt.Sprintf(
						"exported field %s of %s.%s references unexported type %s",
						f.Name(), d.SourcePkg, d.SourceName, refQualified(ref),
					),
				})
			}
		}
	}

	return diags
}

func validateValue(d TaggedDecl) []Diagnostic {
	var diags []Diagnostic

	v, ok := d.Obj.(interface{ Type() types.Type })
	if !ok {
		return diags
	}
	for _, ref := range findUnexportedRefs(v.Type(), nil) {
		diags = append(diags, Diagnostic{
			Severity: SevWarning,
			Pos:      d.Pos,
			Rule:     RuleUnexpValueType,
			Message: fmt.Sprintf(
				"%s %s.%s has unexported type %s in its declared type",
				d.Kind, d.SourcePkg, d.SourceName, refQualified(ref),
			),
		})
	}
	return diags
}

func validateFunc(d TaggedDecl) []Diagnostic {
	var diags []Diagnostic

	fn, ok := d.Obj.(*types.Func)
	if !ok {
		return diags
	}
	sig, _ := fn.Type().(*types.Signature)
	if sig == nil {
		return diags
	}

	seen := map[*types.TypeName]bool{}

	collect := func(t types.Type) {
		for _, ref := range findUnexportedRefs(t, seen) {
			diags = append(diags, Diagnostic{
				Severity: SevError,
				Pos:      d.Pos,
				Rule:     RuleUnexportedSig,
				Message: fmt.Sprintf(
					"signature of func %s.%s mentions unexported type %s; wrapper would not compile",
					d.SourcePkg, d.SourceName, refQualified(ref),
				),
			})
		}
	}

	if sig.TypeParams() != nil {
		for i := 0; i < sig.TypeParams().Len(); i++ {
			collect(sig.TypeParams().At(i).Constraint())
		}
	}
	if sig.Params() != nil {
		for i := 0; i < sig.Params().Len(); i++ {
			collect(sig.Params().At(i).Type())
		}
	}
	if sig.Results() != nil {
		for i := 0; i < sig.Results().Len(); i++ {
			collect(sig.Results().At(i).Type())
		}
	}

	return diags
}

// findUnexportedRefs walks t and returns every named type whose
// identifier is unexported. seen tracks already-visited TypeName
// pointers to handle cyclic types; pass nil if you don't care about
// dedup across multiple call sites.
func findUnexportedRefs(t types.Type, seen map[*types.TypeName]bool) []*types.TypeName {
	if t == nil {
		return nil
	}
	if seen == nil {
		seen = map[*types.TypeName]bool{}
	}
	var out []*types.TypeName
	walkType(t, seen, &out)
	return out
}

func walkType(t types.Type, seen map[*types.TypeName]bool, out *[]*types.TypeName) {
	switch x := t.(type) {
	case *types.Named:
		tn := x.Obj()
		if seen[tn] {
			return
		}
		seen[tn] = true
		if tn.Pkg() != nil && !tn.Exported() {
			*out = append(*out, tn)
		}
		// Recurse into type arguments to catch unexported types used
		// to instantiate exported generics: e.g. List[secret].
		for i := 0; i < x.TypeArgs().Len(); i++ {
			walkType(x.TypeArgs().At(i), seen, out)
		}
	case *types.Alias:
		tn := x.Obj()
		if seen[tn] {
			return
		}
		seen[tn] = true
		if tn.Pkg() != nil && !tn.Exported() {
			*out = append(*out, tn)
		}
		walkType(types.Unalias(x), seen, out)
	case *types.Pointer:
		walkType(x.Elem(), seen, out)
	case *types.Slice:
		walkType(x.Elem(), seen, out)
	case *types.Array:
		walkType(x.Elem(), seen, out)
	case *types.Map:
		walkType(x.Key(), seen, out)
		walkType(x.Elem(), seen, out)
	case *types.Chan:
		walkType(x.Elem(), seen, out)
	case *types.Signature:
		if x.Params() != nil {
			for i := 0; i < x.Params().Len(); i++ {
				walkType(x.Params().At(i).Type(), seen, out)
			}
		}
		if x.Results() != nil {
			for i := 0; i < x.Results().Len(); i++ {
				walkType(x.Results().At(i).Type(), seen, out)
			}
		}
	case *types.Tuple:
		for i := 0; i < x.Len(); i++ {
			walkType(x.At(i).Type(), seen, out)
		}
	case *types.Struct:
		for i := 0; i < x.NumFields(); i++ {
			walkType(x.Field(i).Type(), seen, out)
		}
	case *types.Interface:
		for i := 0; i < x.NumExplicitMethods(); i++ {
			walkType(x.ExplicitMethod(i).Type(), seen, out)
		}
		for i := 0; i < x.NumEmbeddeds(); i++ {
			walkType(x.EmbeddedType(i), seen, out)
		}
	case *types.TypeParam:
		// Constraints are walked at the declaring site to avoid
		// infinite recursion on self-referential constraints like
		// F[T Ordered[T]].
	case *types.Basic:
		// no name to inspect
	}
}

// refQualified renders a TypeName as "pkg.Name" for diagnostic
// messages. Builtins (Pkg() == nil) render as just the name.
func refQualified(tn *types.TypeName) string {
	if tn.Pkg() == nil {
		return tn.Name()
	}
	return tn.Pkg().Path() + "." + tn.Name()
}

// isExportedIdent reports whether id starts with an uppercase letter.
// Mirrors token.IsExported but works on strings we constructed
// ourselves (rename tokens).
func isExportedIdent(id string) bool {
	if id == "" {
		return false
	}
	r := []rune(id)[0]
	return unicode.IsUpper(r)
}

// goVersionLT reports whether version v is strictly less than target.
// Both are dotted strings like "1.24" or "1.24.1".
func goVersionLT(v, target string) bool {
	a := versionTuple(v)
	b := versionTuple(target)
	for i := 0; i < len(a); i++ {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}

func versionTuple(v string) [3]int {
	var t [3]int
	parts := strings.Split(v, ".")
	for i := 0; i < 3 && i < len(parts); i++ {
		fmt.Sscanf(parts[i], "%d", &t[i])
	}
	return t
}
