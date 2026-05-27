# microwave — v0 Specification

## 1. Purpose

`microwave` is a CLI tool that generates a single-file **umbrella package** for a Go module. The umbrella re-exports a curated subset of the module's public API so that downstream consumers can import one path instead of many.

The motivation: in large Go projects the developer experience of locating and importing many internal sub-packages is painful. An umbrella package that covers ~90% of common usage smooths this. `microwave` automates the generation of that umbrella from comment-tagged declarations.

The tool is **opt-in**: nothing is exported unless explicitly tagged.

## 2. Invocation

```
microwave <path>... --out <file> --pkg <name>
```

All flags are **required**. No defaults, no inference.

| Argument / Flag | Meaning                                                                                                                      |
| --------------- | ---------------------------------------------------------------------------------------------------------------------------- |
| `<path>...`     | One or more positional paths to scan (directories or globs, e.g. `./api/...`). No default — paths must be stated explicitly. |
| `--out <file>`  | Path of the generated Go file.                                                                                               |
| `--pkg <name>`  | Package name to declare at the top of the generated file.                                                                    |

There are no subcommands in v0. No config file. No watch mode. No init scaffold.

## 3. Tagging

Re-export is opt-in via a magic comment that is part of the declaration's doc comment:

```go
//microwave:export
// Foo does the thing.
func Foo() {}
```

The tag's exact position inside the doc comment is flexible. It may sit above the human-readable doc lines (the form a person typically writes), or beneath them. Both of the following are equivalent:

```go
//microwave:export
// Foo does the thing.
func Foo() {}
```

```go
// Foo does the thing.
//
//microwave:export
func Foo() {}
```

The second form is what `gofmt` produces from the first: gofmt recognises `//microwave:export` as a directive comment (matching the `//word:rest` shape used by `//go:build`, `//go:generate`, etc.) and normalises directives to sit immediately above the declaration, separated from the rest of the doc by a blank `//` line. Both positions, and any other position inside the comment group, are accepted by microwave.

The tag must be **contiguous** with the declaration: it must live inside the comment group that Go's parser attaches to the decl as its `Doc`. A blank source line between the tag and the declaration detaches the tag from the decl. When the tool encounters such a floating `//microwave:export` line, it emits a warning and ignores the tag.

Only one tag is permitted per declaration. Two tags inside the same comment group is an error.

### 3.1 Rename form

```go
//microwave:export Bar
// Foo does the thing.
func Foo() {}
```

When a second whitespace-separated token follows `export`, it is the exported name in the umbrella. The rename token exists for one reason: resolving collisions between two tagged decls that would otherwise share the same name in the umbrella.

Re-exporting an unexported (lowercase) source declaration is **not supported** — Go's visibility rules prevent the umbrella from referencing identifiers private to another package, so no rename can make this work. Tagging an unexported decl is always an error (see §5.1), regardless of whether a rename token is present.

### 3.2 Scope

The tag applies to **individual declarations only**. There is no file-level or package-level tagging in v0.

### 3.3 Grouped declarations

For `var ( … )`, `const ( … )`, and `type ( … )` blocks:

- **Single-spec group**: `//microwave:export` may sit above the keyword (`var`/`const`/`type`) or above the spec inside; both positions tag the single spec.
- **Multi-spec group**: the tag must sit above the individual spec line inside the block. Placing the tag above the group keyword is an error: _"tag individual decls inside the group, not the group itself"_.

### 3.4 Doc comment propagation

The lines of the comment group _other than_ the tag are copied verbatim into the generated umbrella above the corresponding re-export, preserving godoc on the umbrella surface. The blank `//` separator line that gofmt inserts immediately adjacent to a tag is treated as part of the tag's formatting (not as user-authored doc) and is dropped from the propagated doc.

## 4. Re-export Mechanism

For each tagged declaration, microwave emits the following in the umbrella file:

| Source decl                                     | Umbrella emission                                                               |
| ----------------------------------------------- | ------------------------------------------------------------------------------- |
| Exported type (struct, interface, alias, named) | `type Foo = pkg.Foo`                                                            |
| Generic type                                    | `type Foo[T any] = pkg.Foo[T]` (requires Go ≥ 1.24)                             |
| Exported var                                    | `var Foo = pkg.Foo`                                                             |
| Exported const                                  | `const Foo = pkg.Foo` (redeclaration; const aliases do not exist in Go)         |
| Exported func (generic or not)                  | Wrapper func: `func Foo(...) ... { return pkg.Foo(...) }`. Always a wrapper, never a var alias. Chosen for godoc fidelity — `var F = pkg.F` would render as a variable, losing the signature on the umbrella's godoc page. |
| Method on a re-exported type                    | Rides along with the type alias. No separate emission, no separate tag.         |

The generated file imports each source package under a unique import alias and references identifiers through those aliases.

### 4.1 Wrapper signature rewriting

When emitting a function wrapper, every type that appears in the signature (parameters, returns, generic type parameters and their constraints) is rewritten according to the following rule:

- If the type is **tagged for re-export in this umbrella**, the wrapper uses its **umbrella name** (post-rename).
- Otherwise the wrapper uses the source-qualified name (`pkg.Type`), with the import alias the umbrella has assigned to that package.

This keeps the umbrella's godoc surface self-consistent: a consumer reading the umbrella sees one set of type names rather than a mix of local aliases and `pkg.X` qualifications.

Implementation note: the emitter must build a lookup map keyed by the source-qualified identifier (`<import-path>.<source-name>`) with values equal to the umbrella name (post-rename). The map is populated from the full set of tagged type declarations before any wrapper is emitted. Wrappers may safely forward-reference types defined later in the same umbrella file — Go resolves intra-package references regardless of declaration order.

If a tagged type is later untagged while a wrapper still depends on it, the umbrella will fail to compile. This is intentional — the tool surfaces inconsistency rather than silently re-qualifying.

Parameter names from the source are copied into the wrapper. If the source uses blank or duplicate parameter names (e.g. `func F(_ string, _ int)`), the emitter generates positional names (`a0`, `a1`, …) so the wrapper body can forward them.

## 5. Validation Rules

`microwave` performs the following checks on tagged declarations. Some are **errors** (non-zero exit, no file written); others are **warnings** (printed to stderr, file still generated).

### 5.1 Errors

| Condition                                                                          | Reason                                                                                                                                                                              |
| ---------------------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Zero tagged decls found across all scanned paths                                   | Prevents silent empty umbrella. Error message must list every scanned path so the user can see where the tool looked.                                                               |
| Tagged decl is unexported (lowercase)                                              | Go's visibility rules make the source identifier unreachable from the umbrella package. No rename can fix this, so even a rename token does not rescue the tag.                     |
| Rename token starts with a lowercase letter                                        | Re-exporting under an unexported name in the umbrella defeats the point. The rename token must begin with an uppercase letter.                                                      |
| Two emissions resolve to the same exported name                                    | Collision. User must rename one via the tag's rename token.                                                                                                                         |
| Two or more `//microwave:export` tags in a single comment group                    | Each declaration may carry at most one tag.                                                                                                                                         |
| Any tagged function has unexported types in its parameters or return types         | All funcs are emitted as wrappers (see §4), and a wrapper signature must name every parameter and return type. If the type is unexported in the source package, the wrapper does not compile. Applies equally to generic and non-generic funcs. |
| Go module version (`go.mod`) is below 1.24 and a generic type is tagged            | Generic type aliases require 1.24.                                                                                                                                                  |

All errors must be descriptive: name the offending file, line, and identifier, and state the rule that was violated.

### 5.2 Warnings

| Condition                                                                 | Reason                                                                                                       |
| ------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------ |
| Tagged type has **exported** struct fields whose types are unexported     | Surfaces leaked API but does not block. Unexported (lowercase) fields are ignored — they're already private. Includes embedded fields: an embedded field carries the embedded type's exported name as its field name, so an embedded unexported type counts as an exported field with an unexported type. |
| Tagged var or const has an unexported declared type                       | Value is reachable, type name is not. Emitted with warning.                                                  |
| Generic constraint on a tagged generic decl references an unexported type | Constraint cannot be satisfied from outside. Emitted with warning.                                           |
| Floating `//microwave:export` tag not contiguous with any declaration     | The tag has lost its association with a decl (typically because of a blank line). The tag is ignored. Warning includes file/line so the user can fix it. |

Methods of a re-exported type are **not** re-checked. If the user tags a type, all its methods come along; the tool does not warn about unexported types appearing inside method signatures.

## 6. Scan Rules

- Only the paths explicitly passed on the command line are scanned. No automatic walk of the whole module.
- `_test.go` files are always skipped.
- Packages whose import path contains `/internal/` are always skipped, even if explicitly passed.
- The tool must be run from inside a Go module (a `go.mod` must be discoverable above the current working directory) so it can resolve package import paths.

## 7. Output File

The generated file is a normal Go source file:

```go
// Code generated by microwave. DO NOT EDIT.

//go:generate microwave ./api/... --out umbrella.go --pkg umbrella

package umbrella

import (
    a "github.com/acme/foo/api/a"
    b "github.com/acme/foo/api/b"
)

// Foo does the thing.
type Foo = a.Foo

// Bar holds state.
var Bar = b.Bar
```

Rules:

- Header line is exactly `// Code generated by microwave. DO NOT EDIT.` (matches Go's recognised generated-file pattern).
- The `//go:generate microwave ...` line records the exact invocation so `go generate ./...` reproduces the file.
- No timestamp, no version line, no per-decl provenance comment — to keep diffs minimal.
- Import aliases are deterministic and stable across runs.
- Re-exports are grouped and ordered deterministically (e.g. by emitted name) so regeneration is diff-stable.

### 7.1 Output location

The umbrella file lives wherever `--out` points. There is no `foo/foo.go` convention and no coupling between `--out` and `--pkg`. Valid layouts include:

- Module-wide umbrella at the root: `--out ./umbrella.go --pkg umbrella`
- Subpackage umbrella: `--out ./api/api.go --pkg api`
- A package that itself has subpackages: the umbrella is just one `.go` file in that package's directory; the directory can contain anything else.

The only hard constraint is that the output file must live **inside the current Go module** so that `go build` can resolve the imports of the scanned packages. Pointing `--out` outside the module is a usage error.

## 8. Edge Cases Explicitly Out of Scope for v0

The following are known, intentionally unhandled, and **must be documented** when encountered:

- **Build tags / `//go:build` constraints.** Source files with build constraints are scanned like any other file; the constraint is **not** propagated to the umbrella. If a tagged decl is only present under a specific build tag, the generated umbrella may fail to compile under other tag combinations. Workaround: do not tag decls inside build-tagged files in v0.
- **`cgo` files.** Untested. Behaviour undefined.
- **`iota`-derived consts with non-representable values.** Redeclaration uses `const Foo = pkg.Foo`. If the value is not representable in a const context (e.g. depends on a private typed expression), the generated file will fail to compile. The tool does not detect this in v0.
- **Generic type aliases on Go < 1.24.** Not supported. Errors as listed in §5.1.
- **Renaming to a Go reserved keyword.** Not validated in v0; the generated file will fail to compile.
- **Methods on aliased types whose signatures reference unexported types.** Not checked. May produce an umbrella whose surface mentions unreachable types in method docs.
- **File-level or package-level `//microwave:export` directives.** Not supported. Each decl must be tagged individually.
- **Config file (`microwave.toml`) and CLI subcommands (`init`, `check`, `watch`).** Not in v0.
- **Re-exporting from `internal/` packages.** Always skipped.
- **Multiple modules in one invocation.** Undefined; v0 assumes a single module rooted above the cwd.
- **Scan paths outside the current module.** Undefined. Passing a path that resolves outside the module rooted above the cwd (or under a different `go.mod`) is not supported in v0.

## 9. Exit Codes

| Code | Meaning                                                                                          |
| ---- | ------------------------------------------------------------------------------------------------ |
| 0    | Generation succeeded (warnings may have been printed).                                           |
| 1    | Validation error (see §5.1). No file written.                                                    |
| 2    | Usage error (missing required flags, unreadable path, no `go.mod` found, etc.). No file written. |

## 10. Glossary

- **Umbrella package**: the single-file package generated by microwave that re-exports curated symbols from many source packages.
- **Tag**: a `//microwave:export [NewName]` comment marking a declaration for inclusion.
- **Rename token**: the optional second whitespace-separated token after `export` in a tag, used to resolve collisions by overriding the emitted name.
- **Collision**: two tagged decls that would emit under the same name in the umbrella.
- **Floating tag**: a `//microwave:export` line separated from its declaration by a blank line, so Go's parser does not associate the tag with the decl.
- **gofmt-normalised tag position**: gofmt places `//microwave:export` (a directive-shaped comment) at the bottom of the declaration's doc comment, beneath any human-readable doc lines and after a blank `//` separator. The tool accepts this position and the original above-the-doc position interchangeably.
