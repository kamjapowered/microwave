package microwave

import (
	"go/ast"
	"go/token"
	"go/types"

	"golang.org/x/tools/go/packages"
)

// Kind classifies a tagged declaration. Generic types/funcs share
// the same Kind as their non-generic counterparts; the AST node
// carries the type-parameter list when needed.
type Kind int

const (
	KindType Kind = iota
	KindVar
	KindConst
	KindFunc
)

func (k Kind) String() string {
	switch k {
	case KindType:
		return "type"
	case KindVar:
		return "var"
	case KindConst:
		return "const"
	case KindFunc:
		return "func"
	}
	return "?"
}

// TaggedDecl is the unit of work that flows from scan through
// validate to emit. Each tagged declaration in the source becomes
// one TaggedDecl.
type TaggedDecl struct {
	Kind       Kind
	SourcePkg  string         // import path, e.g. "github.com/acme/foo/api/a"
	SourceName string         // identifier as it appears in the source
	EmitName   string         // identifier in the umbrella (may equal SourceName)
	DocComment string         // text between tag and decl, copied verbatim
	Pos        token.Position // file:line of the tagged decl (for diagnostics)

	// AST + type info handles used by validate and emit.
	Decl ast.Node          // *ast.FuncDecl, *ast.TypeSpec, *ast.ValueSpec
	Obj  types.Object      // resolved object from go/types
	Pkg  *packages.Package // source package
}

// Severity classifies a Diagnostic. SevError halts the pipeline; SevWarning
// is reported but does not stop emission.
type Severity int

const (
	SevWarning Severity = iota
	SevError
)

func (s Severity) String() string {
	switch s {
	case SevWarning:
		return "warning"
	case SevError:
		return "error"
	}
	return "?"
}

// Field is one key/value bullet that appears in the body of a
// Diagnostic. Keys are right-aligned by the UI so multiple fields line
// up beneath the header.
type Field struct {
	Key   string
	Value string
}

// F is a tiny constructor for Field; cuts down the noise at emission
// sites.
func F(key, value string) Field { return Field{Key: key, Value: value} }

// Diagnostic is a single user-facing message produced by any pipeline
// phase. Rule is a short stable id so callers and tests can match on
// it without parsing Summary. Summary is a one-line human-readable
// description; Fields decompose the specifics into key/value bullets.
// The Pos is rendered as an implicit final "at:" field.
type Diagnostic struct {
	Severity Severity
	Pos      token.Position
	Rule     string
	Summary  string
	Fields   []Field
}

// HasErrors reports whether any diagnostic in d carries SevError.
func HasErrors(d []Diagnostic) bool {
	for _, x := range d {
		if x.Severity == SevError {
			return true
		}
	}
	return false
}

// Rule identifiers. Each rule mentioned in spec §5 (and a few internal
// rules) has a stable string here.
const (
	// Errors.
	RuleNoTags         = "no-tags"          // §5.1: zero tagged decls across all paths
	RuleUnexportedDecl = "unexported-decl"  // §5.1: tagged decl is unexported (lowercase)
	RuleLowercaseRen   = "lowercase-rename" // §5.1: rename token begins lowercase
	RuleCollision      = "collision"        // §5.1: two emissions share a name
	RuleUnexportedSig  = "unexported-sig"   // §5.1: func signature mentions an unexported type
	RuleGenericPre124  = "generic-pre-124"  // §5.1: generic type tagged on go.mod < 1.24
	RuleTagOnGroup     = "tag-on-group"     // §3.3: tag above keyword of a multi-spec group
	RuleTagMalformed   = "tag-malformed"    // tag line is not //microwave:export[ NAME]
	RuleMethodTag      = "method-tag"       // tagging a method is invalid; tag the type
	RuleMultiNameSpec  = "multi-name-spec"  // var/const spec with multiple names
	RuleBlankDecl      = "blank-decl"       // tagged spec name is "_"
	RuleMultiTag       = "multi-tag"        // §3: more than one tag in a single comment group
	RuleLoadError      = "load-error"       // packages.Load returned an error
	RuleEmitFormat     = "emit-format"      // gofmt failed on emitted bytes
	RuleOutsideModule  = "outside-module"   // --out resolves outside the module
	RuleUsage          = "usage"            // generic CLI usage failure

	// Warnings.
	RuleUnexpFieldType  = "unexported-field-type" // §5.2
	RuleUnexpConstraint = "unexported-constraint" // §5.2
	RuleFloatingTag     = "floating-tag"          // §3: tag not contiguous with a decl
	RuleExcludeNoMatch  = "exclude-no-match"      // --exclude pattern matched no packages
)
