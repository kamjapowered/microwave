# microwave — v0 Implementation Plan

This document is the implementation blueprint for the spec in [`summary.md`](./summary.md). It is meant to be reviewable in isolation by a second pair of eyes who has read the spec but not the design conversation.

## 1. Stack

| Concern | Choice | Rationale |
| --- | --- | --- |
| Language | Go (matches the project's domain) | Required — we use `go/ast`, `go/types`, `go/format`. |
| Min Go version (scanned module) | 1.24 | Generic type aliases (spec §4) require it. The user's `go.mod` directive is the one enforced by the `generic-pre-124` rule. |
| Min Go version (microwave itself) | 1.25 | Pulled in transitively by current `golang.org/x/tools`. Not user-visible — only affects building microwave from source. |
| Parsing + type checking | `golang.org/x/tools/go/packages` | Standard for Go codegen tools (stringer, mockgen, sqlc). Gives parsed AST + resolved types + import paths from one `Load()`. Maintained by the Go team. |
| AST walking | `go/ast` (stdlib) | Comes bundled with the parsed files returned by `packages.Load`. |
| Type resolution | `go/types` (stdlib) | Lets us answer "is this type exported", "what package does this type come from". |
| Source emission | string builder → `go/format.Source` | Standard codegen pattern. Building `*ast.File` programmatically is doable but verbose; not worth it for v0. |
| CLI flag parsing | `github.com/spf13/cobra` | Matches the convention of sibling CLIs in this codebase (`cahier`, `see`, `tin`). |
| Error accumulation | `errors.Join` (stdlib) | Per-phase collection. |

External dependencies: `github.com/spf13/cobra` (CLI) and `golang.org/x/tools` (`go/packages`). No others planned for v0.

## 2. Package Layout

The project follows the same shape as the other CLIs in this codebase (`cahier`, `see`, `tin`): a tiny root `main.go`, cobra commands under `cmd/`, and the actual logic under `internal/`. The module name is the bare project name (`module microwave`), matching the convention used in sibling projects.

For v0, the logic lives in a single internal package. Splitting scan/validate/emit into separate packages is premature for the scope of this tool; one package with a file per phase is enough to keep things readable.

```
main.go                    package main — calls cmd.Execute()

cmd/
    root.go                cobra root command, flag definitions, Execute()

internal/microwave/
    model.go               TaggedDecl, Kind, Severity, Diagnostic, rule-id constants
    scan.go                packages.Load, AST walk, tag parsing, floating-tag detection
    validate.go            build lookup map, apply §5.1 errors / §5.2 warnings
    emit.go                import aliasing + per-kind emission + gofmt + write

go.mod
go.sum
```

Rationale:

- One internal package keeps the file count low for v0; we can split later if any single file balloons past a few hundred lines.
- `scan` does I/O + AST work but no validation logic; outputs raw `TaggedDecl`s.
- `validate` is pure: takes `[]TaggedDecl`, returns `(lookupMap, []Diagnostic)`. Easy to unit-test.
- `emit` is also pure given a validated set + the lookup map; gofmt + write happens at the very end.

External dependencies:

- `github.com/spf13/cobra` for the CLI surface (matches sibling projects).
- `golang.org/x/tools` for `go/packages`.

## 3. Data Model

```go
// internal/microwave

type Kind int

const (
    KindType Kind = iota   // includes generic types
    KindVar
    KindConst
    KindFunc               // includes generic funcs
)

type TaggedDecl struct {
    Kind        Kind
    SourcePkg   string         // import path, e.g. "github.com/acme/foo/api/a"
    SourceName  string         // identifier as it appears in the source
    EmitName    string         // identifier in the umbrella (may equal SourceName)
    DocComment  string         // text between tag and decl, copied verbatim
    Pos         token.Position // file:line of the tagged decl (for diagnostics)

    // AST + type info handles — needed by validate and emit
    Decl        ast.Node       // *ast.FuncDecl, *ast.TypeSpec, *ast.ValueSpec
    Obj         types.Object   // resolved object from go/types
    Pkg         *packages.Package
}

type Severity int
const (
    SevError Severity = iota
    SevWarning
)

type Diagnostic struct {
    Severity Severity
    Pos      token.Position
    Rule     string  // short rule id, e.g. "collision", "unexported-sig"
    Message  string  // human-readable, includes file/line/identifier
}
```

`Diagnostic.Rule` is a stable short id so callers (and tests) can match on it without parsing the message. Spec §5 rules each get a constant in `model.go`.

## 4. Pipeline

`main.go` runs three phases sequentially. Each phase collects diagnostics and returns. Errors in one phase halt the pipeline; warnings flow through.

```
parse flags
  └─> scan(paths)            → ([]TaggedDecl, []Diagnostic)
        errors? print all, exit 1
  └─> validate(decls)        → (lookup map, []Diagnostic)
        errors? print all, exit 1
  └─> emit(decls, map, out)  → []Diagnostic
        errors? print all, exit 1
print warnings to stderr
exit 0
```

### 4.1 Phase: scan

Inputs: `[]string` paths from CLI, plus the resolved module info.

Steps:
1. Resolve `go.mod` location by walking up from `cwd`. If none found → usage error (exit 2).
2. Validate `--out` resolves to a path **inside** the module root. Otherwise usage error.
3. Build a `packages.Config` with:
   - `Mode = NeedName | NeedFiles | NeedSyntax | NeedTypes | NeedTypesInfo | NeedImports | NeedDeps`
   - `Dir = cwd`
   - `Fset = token.NewFileSet()`
4. Call `packages.Load(cfg, paths...)`.
5. Reject any package whose import path contains `/internal/` (skip silently; spec §6).
6. For each remaining package, for each `*ast.File`, walk top-level decls:
   - For each `*ast.FuncDecl`, inspect `Doc` for a tag line.
   - For each `*ast.GenDecl`:
     - If exactly one spec: inspect `GenDecl.Doc` and the spec's own `Doc`.
     - If multiple specs: inspect each spec's `Doc` only. A tag on `GenDecl.Doc` of a multi-spec group → diagnostic, rule `tag-on-group`.
7. For each tag found, build a `TaggedDecl`. Capture:
   - Source name (from the AST node).
   - Emit name (from rename token; fall back to source name).
   - Doc comment text (everything after the tag line within the same comment group).
   - `types.Object` via `pkg.TypesInfo.Defs[ident]`.
   - `Kind` from AST node type.
8. After processing every decl, walk every `*ast.File`'s `Comments` field to find **floating tags**. For each comment group that is _not_ the `Doc` of any decl/spec, scan its lines for any starting with `//microwave:export`. Each such line emits a warning with rule `floating-tag` and is otherwise ignored. (Attached groups have already been handled in step 7; tags inside them at any position are valid.)
9. Return all `TaggedDecl`s and all diagnostics.

Tag parsing:
- Line must match `^//microwave:export(\s+(\S+))?\s*$`.
- One optional whitespace-separated token = rename.
- Anything else (e.g. extra tokens) → diagnostic, rule `tag-malformed`.

Tag location inside an attached `CommentGroup`:
- The tag may appear at any position in the group (first line, last line, or in the middle). gofmt detects `//microwave:export` as a directive comment and moves it to the bottom of the doc group with a blank `//` separator before it; the scanner accepts both the pre-gofmt and post-gofmt positions.
- The scanner walks every line of the attached comment group, collecting any line that begins with `//microwave:export`. Exactly one such well-formed line per group is required; zero means the decl is untagged; two or more means rule `multi-tag` (error).
- Doc-comment extraction: every line of the group except the tag is concatenated with `\n`. The blank `//` separator line that gofmt inserts immediately adjacent to the tag is dropped (so the propagated doc has no orphan blank line). All other doc lines are copied verbatim (including their leading `// `).
- A blank source line between the tag's comment group and the decl splits them into two separate `CommentGroup`s. The first group then becomes unattached and is reported via the floating-tag warning (see step 8).

Load errors are fatal. If `packages.Load` returns any package with a non-empty `Errors` slice, scan reports each as a diagnostic with rule `load-error` and the pipeline halts. Partial type info from a failed load is not usable for signature rewriting, so we always fail fast (exit 1).

### 4.2 Phase: validate

Pure function over `[]TaggedDecl`.

Two sub-tasks:

**a. Build the lookup map.**
- Map key: `<import-path>.<source-name>`.
- Map value: emit name.
- Built only from `TaggedDecl`s with `Kind == KindType` (only types participate in signature rewriting; vars/consts/funcs don't appear in signatures).
- Collision detection happens here too: build a second map keyed by emit name to source decl. Second insert with the same emit name → diagnostic, rule `collision`, message names both source positions.

**b. Apply rules.**

For each `TaggedDecl`:

| Check | When | Severity | Rule id |
| --- | --- | --- | --- |
| Source name lowercase (unexported decl tagged) | always | error | `unexported-decl` |
| Emit name lowercase (rename token starts lowercase) | rename token present | error | `lowercase-rename` |
| Two emissions resolve to the same emit name | always | error | `collision` |
| Two or more well-formed tags in one comment group | scan time | error | `multi-tag` |
| Func has unexported type in params or returns (incl. generic constraints) | `Kind == KindFunc` | error | `unexported-sig` |
| Generic type tagged but module Go version < 1.24 | `Kind == KindType` and has type params | error | `generic-pre-124` |
| Tagged type has exported struct field (incl. embedded) of unexported type | `Kind == KindType` and underlying is struct | warning | `unexported-field-type` |
| Var/const declared type is unexported | `Kind == KindVar` or `KindConst` | warning | `unexported-value-type` |
| Generic type constraint references unexported type | `Kind == KindType` with type params | warning | `unexported-constraint` |

Field exportedness uses Go's standard rule: a field is exported iff its name starts uppercase. Embedded fields take their name from the embedded type's identifier. Validation walks `*types.Struct`; for every field whose `Exported()` is true, it inspects the field's type and emits `unexported-field-type` for each unexported named type referenced (via the shared `findUnexportedRefs` helper). The tagged type is still emitted — the warning surfaces leaked surface area without blocking.

Detecting "unexported type" via `go/types`:
- For a `types.Type`, walk to its `*types.Named` or `*types.TypeParam` leaves.
- For each `*types.Named`, ask `t.Obj().Pkg()` and `t.Obj().Exported()`.
  - Different package and `!Exported()` → unexported.
  - Same package as caller and `!Exported()` → unexported.
- For aliases, resolve via `types.Unalias`.
- For `*types.Interface`, structural — no name to check.
- For `*types.TypeParam`, check its constraint (the underlying interface).

A helper `findUnexportedRefs(t types.Type) []*types.Named` walks the type and returns every unexported named reference. Reused by all the validation rules.

Returns: `(lookupMap, []Diagnostic)`.

### 4.3 Phase: emit

Pure function: `emit(decls, lookup) ([]byte, error)`, then I/O writes the bytes.

**Step 1: Group decls by emission section.**

Emit order in the output file (deterministic):
1. Header + go:generate line + package decl
2. Import block
3. Types (sorted by emit name)
4. Vars (sorted by emit name)
5. Consts (sorted by emit name)
6. Funcs (sorted by emit name)

Grouping makes the file readable and makes diffs cluster predictably.

**Step 2: Assign import aliases.**

Walk all `TaggedDecl`s; collect every source package path needed, plus every package referenced by a func wrapper's signature (after rewriting). The set is finite and known before emission.

Alias scheme: take the last path segment, lowercase. If two packages share a segment, append a numeric suffix (`a`, `a2`, `a3`) in import-path-sorted order. Deterministic.

**Step 3: Per-kind emission.**

*Type alias* (`types.go`):
- Non-generic: `type EmitName = aliasN.SourceName`
- Generic: copy the type parameter list verbatim from the source AST, then `= aliasN.SourceName[T1, T2, …]` referencing the same params.

*Var alias* (`vars.go`):
- `var EmitName = aliasN.SourceName`

*Const redeclaration* (`consts.go`):
- `const EmitName = aliasN.SourceName`
- Constant kind/value comes through Go's compiler automatically.

*Func wrapper* (`funcs.go`):
- Emit `func EmitName[<typeparams>](<rewritten params>) (<rewritten returns>) { return aliasN.SourceName[<typeparam idents>](<param idents>) }`.
- If the source has no return values, drop the `return`.
- Parameter name handling:
  - If the source param has a name and it's non-blank: keep it.
  - If blank (`_`) or shared (e.g. `func F(a, b int)` — both params share one ident node): generate positional names `a0`, `a1`, …
- Variadic parameters: emit as `args ...T` and forward as `args...`.

*Signature rewriting* (used by `funcs.go`):
- Walk the source AST's `*ast.Field` types for params and returns.
- For each `*ast.Ident` or `*ast.SelectorExpr` that resolves to a named type:
  - Look up `<source-pkg>.<source-name>` in the lookup map.
  - Hit → write the umbrella's emit name (no qualifier).
  - Miss → write `<alias>.<source-name>` using the import alias for the source package of that type.
- Pre-declared types (`string`, `int`, etc.) and locally-defined-in-source-pkg types are handled by the same logic — `types.Object.Pkg()` returns nil for builtins, telling us not to qualify.
- Function types, channel types, pointer types, slice/map types, struct/interface literals: recurse into element types.

**Step 4: Doc comments.**

For each emitted decl, write the captured doc comment (verbatim) immediately above. Blank line between consecutive decls.

**Step 5: Gofmt.**

After concatenating header + imports + all sections, run `go/format.Source(buf)`. If it returns an error, that's an internal bug (we emitted invalid Go) — surface as `emit-format` diagnostic with the formatter's error.

**Step 6: Write.**

Write the formatted bytes to `--out`, creating parent dirs only if they already exist (no surprise `mkdir -p`). On write error → diagnostic, exit 1.

## 5. Error Handling

- `errors.Join` accumulates diagnostics within a phase.
- A phase returns `([]Diagnostic, error)` where `error` is only used for "I cannot continue at all" cases (failed `go.mod` discovery, failed package load). Validation/emit issues live in the diagnostics slice.
- Main loop after each phase:
  ```go
  diags, err := phase(...)
  printDiagnostics(diags)            // warnings + errors, deterministic order
  if err != nil || anyError(diags) {
      os.Exit(1)
  }
  ```
- Diagnostic output format: `microwave: <file>:<line>:<col>: <severity>: <message> [<rule>]`. The trailing `[<rule>]` lets tooling grep for specific rule classes.

## 6. Determinism

Required because the file is checked into VCS and regenerated; spurious diffs are noise.

- Emit order: sections fixed (types/vars/consts/funcs), within each section sort by emit name (Unicode codepoint order).
- Import aliases: derived from sorted import paths, so adding/removing a package only renames the affected one.
- Doc comments: copied verbatim, no reflow.
- No timestamp, no version, no per-decl provenance.

Test strategy: golden-file test that runs the tool against a fixture module twice and asserts byte equality.

## 7. Concurrency

None in v0. `packages.Load` is already parallel internally. Sequential per-phase processing keeps diagnostic order deterministic. Add concurrency only if profiling shows a real bottleneck.

## 8. Testing Plan

- **Unit tests** (table-driven) per validation rule. Each rule constant gets a fixture pair (passing + failing).
- **Unit test** for tag parsing: `//microwave:export`, `//microwave:export Foo`, `//microwave:export foo` (error), trailing whitespace, extra tokens.
- **Unit test** for `findUnexportedRefs`: various `types.Type` shapes.
- **Unit test** for import alias assignment: known input set → expected aliases.
- **Golden-file integration tests**: small fixture modules under `internal/scan/testdata/`. Each fixture has a `want.go` that the emitter must reproduce exactly.
- **Round-trip test**: scan → validate → emit → invoke `go build` on the output. Catches "emitter produced syntactically valid but semantically broken Go".

## 9. Known Risks and Open Questions for Review

1. **Doc comment / tag overlap with linters.** Putting `//microwave:export` above a doc comment may confuse linters that expect the first line of the comment group to begin the godoc text. We accept this; the source godoc shows the tag as a leading line. Worth a reviewer's opinion.

2. **Wrapper for a func with variadic + unexported type.** Edge case: `func F(args ...internal.T)`. The wrapper would be `func F(args ...internal.T)` — fails the `unexported-sig` rule, so it errors out before emission. No special handling needed. Verify.

3. **Generic func wrappers and constraint type inference.** When emitting `func F[T any](x T) T { return aliasN.F[T](x) }`, we explicitly pass the type parameter to the source call. Go can usually infer it, but explicit is safer and free.

4. **Self-referential generic constraints.** `func F[T Ordered[T]](...)` — the constraint references the type param being defined. Rewriting must not lose the back-reference. Test fixture required.

5. **Output path inside a `go` workspace (`go.work`).** v0 assumes a single `go.mod`. Workspaces are out of scope per spec §8 — but verify the tool exits cleanly with a usage error rather than producing broken output.

6. **Performance on large modules.** `NeedDeps` causes the loader to type-check the full dependency closure. For huge modules this is slow. We need it to resolve cross-package type references in signatures. Lazy loading is deferred — accept the cost for v0 and revisit if profiling shows a real bottleneck.

## 10. Milestones

A suggested implementation order:

1. Root `main.go` + `cmd/root.go` with cobra flag parsing only; no phases. Exit 2 on missing required flags. Verify CLI shape.
2. `internal/scan` against a single hand-written fixture package. Print discovered `TaggedDecl`s.
3. `internal/validate` rules one at a time, each with a fixture.
4. `internal/emit` for types only; verify gofmt + write.
5. Extend `emit` to vars, consts.
6. Func wrappers + signature rewriting (the longest piece).
7. Generic types and funcs.
8. Golden-file integration tests across a multi-package fixture module.
9. Round-trip test invoking `go build` on the generated umbrella.
