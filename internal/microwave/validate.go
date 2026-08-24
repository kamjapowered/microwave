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
				Summary:  "two tagged decls share an umbrella name",
				Fields: []Field{
					F("emit name", d.EmitName),
					F("first", first.SourcePkg+"."+first.SourceName),
					F("first at", first.Pos.String()),
					F("fix", "rename one via the tag's rename token"),
				},
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
			Summary:  "tagged decl is unexported",
			Fields: []Field{
				F("kind", d.Kind.String()),
				F("name", d.SourcePkg+"."+d.SourceName),
			},
		})
	}
	if d.EmitName != d.SourceName && !isExportedIdent(d.EmitName) {
		diags = append(diags, Diagnostic{
			Severity: SevError,
			Pos:      d.Pos,
			Rule:     RuleLowercaseRen,
			Summary:  "rename token must start with an uppercase letter",
			Fields:   []Field{F("got", d.EmitName)},
		})
	}

	switch d.Kind {
	case KindType:
		diags = append(diags, validateType(d, modGo)...)
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
			Summary:  "generic type alias requires Go 1.24+",
			Fields: []Field{
				F("type", d.SourcePkg+"."+d.SourceName),
				F("go.mod", modGo),
			},
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
						Summary:  "generic constraint references an unexported type",
						Fields: []Field{
							F("type", d.SourcePkg+"."+d.SourceName),
							F("references", refQualified(ref)),
						},
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
					Summary:  "exported struct field has an unexported type",
					Fields: []Field{
						F("type", d.SourcePkg+"."+d.SourceName),
						F("field", f.Name()),
						F("field type", refQualified(ref)),
					},
				})
			}
		}
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
				Summary:  "signature uses an unexported type",
				Fields: []Field{
					F("func", d.SourcePkg+"."+d.SourceName),
					F("type", refQualified(ref)),
				},
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
		// An alias is exposed to consumers under its own name. If the
		// alias name is exported, the wrapper can spell it out and
		// compile — the underlying type stays hidden behind the alias.
		// We deliberately do NOT recurse into types.Unalias here:
		// doing so would flag the underlying type even when the user
		// has explicitly aliased it under an exported name (e.g.
		// `type Entity = entity`), which is exactly the use case
		// aliases exist for.
		tn := x.Obj()
		if seen[tn] {
			return
		}
		seen[tn] = true
		if tn.Pkg() != nil && !tn.Exported() {
			*out = append(*out, tn)
		}
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
