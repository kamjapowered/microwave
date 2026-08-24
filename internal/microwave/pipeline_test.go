package microwave

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runInFixture writes the given files into a fresh module rooted at
// t.TempDir(), chdirs there for the duration of the test, then invokes
// Run with the supplied paths. Returns the contents of the resulting
// umbrella file (if any) and the captured stderr.
func runInFixture(t *testing.T, files map[string]string, paths []string) (string, string, error) {
	t.Helper()
	dir := t.TempDir()
	if _, ok := files["go.mod"]; !ok {
		files["go.mod"] = "module testmod\n\ngo 1.24\n"
	}
	for rel, content := range files {
		abs := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
			t.Fatalf("WriteFile %s: %v", rel, err)
		}
	}

	old, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })

	var stderr bytes.Buffer
	cfgArgs := append([]string{}, paths...)
	cfgArgs = append(cfgArgs, "--out", "umbrella.go", "--pkg", "umbrella")

	runErr := Run(Config{
		Paths:  paths,
		Out:    "umbrella.go",
		Pkg:    "umbrella",
		Args:   cfgArgs,
		Stderr: &stderr,
	})

	var out string
	if data, ferr := os.ReadFile(filepath.Join(dir, "umbrella.go")); ferr == nil {
		out = string(data)
	}
	return out, stderr.String(), runErr
}

func TestPipeline_HappyPath_Compiles(t *testing.T) {
	files := map[string]string{
		"api/a/a.go": `package a

//microwave:export
// Foo does the thing.
type Foo struct{ Name string }

//microwave:export
// Bar holds state.
var Bar = 42

//microwave:export
// Hello says hi.
func Hello(name string) string { return "hi " + name }
`,
		"api/b/b.go": `package b

//microwave:export
// Baz is constant.
const Baz = "baz"

//microwave:export
// Variadic takes many.
func Variadic(args ...string) int { return len(args) }
`,
	}
	out, stderr, err := runInFixture(t, files, []string{"./api/..."})
	if err != nil {
		t.Fatalf("Run: %v\nstderr:\n%s", err, stderr)
	}

	checks := []string{
		"package umbrella",
		"// Foo does the thing.",
		"type Foo = a.Foo",
		"var Bar = a.Bar",
		"const Baz = b.Baz",
		"func Hello(name string) string { return a.Hello(name) }",
		"func Variadic(args ...string) int { return b.Variadic(args...) }",
		`//go:generate microwave ./api/... --out umbrella.go --pkg umbrella`,
		// bare imports (no alias prefix) when the declared package
		// name is unique
		`"testmod/api/a"`,
		`"testmod/api/b"`,
		// xp.go-style banners between source packages
		"// a\n",
		"// b\n",
	}
	for _, want := range checks {
		if !strings.Contains(out, want) {
			t.Errorf("umbrella missing %q\nfull output:\n%s", want, out)
		}
	}
	// imports should NOT be aliased now that names are unique
	if strings.Contains(out, `a "testmod/api/a"`) {
		t.Errorf("expected bare import, got alias prefix:\n%s", out)
	}

	cmd := exec.Command("go", "build", "./...")
	cmd.Env = append(os.Environ(), "GOWORK=off")
	out2, gerr := cmd.CombinedOutput()
	if gerr != nil {
		t.Fatalf("go build on emitted umbrella failed: %v\n%s\n--- umbrella ---\n%s", gerr, out2, out)
	}
}

func TestPipeline_GenericTypeArgInference(t *testing.T) {
	files := map[string]string{
		"api/a/a.go": `package a

//microwave:export
func Pick[T comparable](x, y T) T { return x }

//microwave:export
func Make[U any]() U { var z U; return z }

//microwave:export
func Mixed[A, B any](x A) B { var z B; _ = x; return z }
`,
	}
	out, stderr, err := runInFixture(t, files, []string{"./api/..."})
	if err != nil {
		t.Fatalf("Run: %v\nstderr:\n%s", err, stderr)
	}

	cases := []struct {
		name string
		want string
	}{
		{"T inferred from args", "return a.Pick(x, y)"},
		{"U not inferable, kept", "return a.Make[U]()"},
		{"B not in params, both kept", "return a.Mixed[A, B](x)"},
	}
	for _, c := range cases {
		if !strings.Contains(out, c.want) {
			t.Errorf("%s: missing %q\nfull output:\n%s", c.name, c.want, out)
		}
	}

	cmd := exec.Command("go", "build", "./...")
	cmd.Env = append(os.Environ(), "GOWORK=off")
	if out2, gerr := cmd.CombinedOutput(); gerr != nil {
		t.Fatalf("go build failed: %v\n%s\n--- umbrella ---\n%s", gerr, out2, out)
	}
}

func TestPipeline_Determinism(t *testing.T) {
	files := map[string]string{
		"api/a/a.go": `package a

//microwave:export
type Foo struct{}

//microwave:export
var Bar = 1

//microwave:export
const Baz = 2

//microwave:export
func Qux() {}
`,
	}
	out1, _, err := runInFixture(t, files, []string{"./api/..."})
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	out2, _, err := runInFixture(t, files, []string{"./api/..."})
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if out1 != out2 {
		t.Errorf("emit is non-deterministic\nfirst:\n%s\nsecond:\n%s", out1, out2)
	}
}

func TestPipeline_NoTags_IsError(t *testing.T) {
	files := map[string]string{
		"api/a/a.go": `package a

// Foo has no tag.
type Foo struct{}
`,
	}
	_, stderr, err := runInFixture(t, files, []string{"./api/..."})
	if err == nil {
		t.Fatal("expected pipeline error, got nil")
	}
	if !strings.Contains(stderr, "no //microwave:export tags found") {
		t.Errorf("expected no-tags message in stderr, got:\n%s", stderr)
	}
}

func TestPipeline_UnexportedDecl_Errors(t *testing.T) {
	files := map[string]string{
		"api/a/a.go": `package a

//microwave:export Foo
type foo int
`,
	}
	_, stderr, err := runInFixture(t, files, []string{"./api/..."})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(stderr, RuleUnexportedDecl) {
		t.Errorf("expected %s rule in stderr, got:\n%s", RuleUnexportedDecl, stderr)
	}
}

func TestPipeline_Collision_Errors(t *testing.T) {
	files := map[string]string{
		"api/a/a.go": `package a

//microwave:export
type Foo struct{}
`,
		"api/b/b.go": `package b

//microwave:export Foo
type Other struct{}
`,
	}
	_, stderr, err := runInFixture(t, files, []string{"./api/..."})
	if err == nil {
		t.Fatal("expected collision error")
	}
	if !strings.Contains(stderr, RuleCollision) {
		t.Errorf("expected %s rule in stderr, got:\n%s", RuleCollision, stderr)
	}
}

func TestPipeline_FloatingTag_Warns(t *testing.T) {
	files := map[string]string{
		"api/a/a.go": `package a

//microwave:export
type Real struct{}

//microwave:export

// detached has a blank line before it
type Detached struct{}
`,
	}
	_, stderr, err := runInFixture(t, files, []string{"./api/..."})
	if err != nil {
		t.Fatalf("Run: %v\nstderr:\n%s", err, stderr)
	}
	if !strings.Contains(stderr, RuleFloatingTag) {
		t.Errorf("expected %s warning in stderr, got:\n%s", RuleFloatingTag, stderr)
	}
}

func TestPipeline_UnexportedSig_Errors(t *testing.T) {
	files := map[string]string{
		"api/a/a.go": `package a

type secret struct{}

//microwave:export
func F(s secret) {}
`,
	}
	_, stderr, err := runInFixture(t, files, []string{"./api/..."})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(stderr, RuleUnexportedSig) {
		t.Errorf("expected %s rule, got:\n%s", RuleUnexportedSig, stderr)
	}
}

func TestPipeline_StaleUmbrella_RegenAfterRename(t *testing.T) {
	files := map[string]string{
		"api/a/a.go": `package a

//microwave:export
type Foo struct{}
`,
	}
	if _, _, err := runInFixture(t, files, []string{"./api/..."}); err != nil {
		t.Fatalf("first Run: %v", err)
	}

	// Rename Foo -> Renamed in source. The umbrella from the first
	// run still references Foo — without the stash-and-delete fix,
	// the second Run would fail with a load error.
	files["api/a/a.go"] = `package a

//microwave:export
type Renamed struct{}
`
	out, stderr, err := runInFixture(t, files, []string{"./api/..."})
	if err != nil {
		t.Fatalf("second Run: %v\nstderr:\n%s", err, stderr)
	}
	if !strings.Contains(out, "type Renamed = a.Renamed") {
		t.Errorf("regenerated umbrella missing Renamed:\n%s", out)
	}
	if strings.Contains(out, "Foo") {
		t.Errorf("regenerated umbrella still references Foo:\n%s", out)
	}
}

func TestPipeline_StaleUmbrella_RestoredOnFailure(t *testing.T) {
	dir := t.TempDir()
	mustWrite := func(rel, content string) {
		t.Helper()
		abs := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite("go.mod", "module testmod\n\ngo 1.24\n")
	mustWrite("api/a/a.go", `package a

//microwave:export
type Foo struct{}
`)

	old, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })

	cfgArgs := []string{"./api/...", "--out", "umbrella.go", "--pkg", "umbrella"}
	mkCfg := func() Config {
		return Config{
			Paths:  []string{"./api/..."},
			Out:    "umbrella.go",
			Pkg:    "umbrella",
			Args:   cfgArgs,
			Stderr: io.Discard,
		}
	}

	if err := Run(mkCfg()); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	original, err := os.ReadFile("umbrella.go")
	if err != nil {
		t.Fatalf("read original umbrella: %v", err)
	}

	// Break the source: tag a func that takes an unexported type.
	// Validation will reject it, so the pipeline must restore the
	// original umbrella instead of leaving the user with no file.
	mustWrite("api/a/a.go", `package a

type secret struct{}

//microwave:export
func Broken(s secret) {}
`)

	var stderr bytes.Buffer
	cfg := mkCfg()
	cfg.Stderr = &stderr
	if err := Run(cfg); err == nil {
		t.Fatalf("expected pipeline error, got nil. stderr:\n%s", stderr.String())
	}
	restored, err := os.ReadFile("umbrella.go")
	if err != nil {
		t.Fatalf("umbrella was lost after failed run: %v\nstderr:\n%s", err, stderr.String())
	}
	if string(restored) != string(original) {
		t.Errorf("umbrella was modified despite pipeline failure\noriginal:\n%s\nrestored:\n%s",
			original, restored)
	}
}

func TestPipeline_FacadeImporter_AutoSkipped(t *testing.T) {
	dir := t.TempDir()
	mustWrite := func(rel, content string) {
		t.Helper()
		abs := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite("go.mod", "module testmod\n\ngo 1.24\n")
	mustWrite("pkg/producer/p.go", `package producer

//microwave:export
type Foo struct{}
`)
	// consumer imports the umbrella being generated and carries its own
	// tag. A re-exporter cannot scan a package that consumes its own
	// output, so consumer is auto-skipped: regeneration succeeds with no
	// --exclude, and consumer's Bar never reaches the umbrella.
	mustWrite("pkg/consumer/c.go", `package consumer

import "testmod/umbrella"

//microwave:export
type Bar struct{}

func Use() umbrella.Foo { return umbrella.Foo{} }
`)
	mustWrite("umbrella/umbrella.go", `// Code generated by microwave. DO NOT EDIT.

package umbrella

import "testmod/pkg/producer"

type Foo = producer.Foo
`)

	old, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })

	var stderr bytes.Buffer
	cfg := Config{
		Paths:  []string{"./pkg/..."},
		Out:    "umbrella/umbrella.go",
		Pkg:    "umbrella",
		Args:   []string{"./pkg/...", "--out", "umbrella/umbrella.go", "--pkg", "umbrella"},
		Stderr: &stderr,
	}

	// No --exclude needed: the facade importer self-excludes.
	if err := Run(cfg); err != nil {
		t.Fatalf("expected auto-skip to let regeneration succeed without --exclude; stderr:\n%s", stderr.String())
	}
	out, err := os.ReadFile("umbrella/umbrella.go")
	if err != nil {
		t.Fatalf("read umbrella: %v", err)
	}
	if !strings.Contains(string(out), "type Foo = producer.Foo") {
		t.Errorf("umbrella missing producer.Foo:\n%s", out)
	}
	if strings.Contains(string(out), "Bar") {
		t.Errorf("consumer imports the umbrella and must be auto-skipped; its tag leaked:\n%s", out)
	}
}

func TestPipeline_Exclude_SubtractsTaggedPackage(t *testing.T) {
	dir := t.TempDir()
	mustWrite := func(rel, content string) {
		t.Helper()
		abs := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite("go.mod", "module testmod\n\ngo 1.24\n")
	mustWrite("pkg/keep/k.go", `package keep

//microwave:export
type Foo struct{}
`)
	// optional is a normal tagged package that does NOT import the
	// umbrella, so auto-skip leaves it in; --exclude must still subtract
	// it so its tag stays out of the generated file.
	mustWrite("pkg/optional/o.go", `package optional

//microwave:export
type Bar struct{}
`)

	old, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })

	cfg := Config{
		Paths:    []string{"./pkg/..."},
		Excludes: []string{"./pkg/optional"},
		Out:      "umbrella.go",
		Pkg:      "umbrella",
		Args:     []string{"./pkg/...", "--exclude", "./pkg/optional", "--out", "umbrella.go", "--pkg", "umbrella"},
		Stderr:   io.Discard,
	}
	if err := Run(cfg); err != nil {
		t.Fatalf("Run with --exclude: %v", err)
	}
	out, err := os.ReadFile("umbrella.go")
	if err != nil {
		t.Fatalf("read umbrella: %v", err)
	}
	if !strings.Contains(string(out), "type Foo = keep.Foo") {
		t.Errorf("umbrella missing keep.Foo:\n%s", out)
	}
	if strings.Contains(string(out), "Bar") {
		t.Errorf("--exclude failed to subtract pkg/optional:\n%s", out)
	}
}

func TestPipeline_ExportedAliasOfUnexported_AllowedInSignature(t *testing.T) {
	files := map[string]string{
		"ecs/ecs.go": `package ecs

// entity is private; Entity is an exported alias to it.
type entity uint64

//microwave:export
type Entity = entity

//microwave:export
type World struct{}

//microwave:export
func AddComponent[T any](w *World, e Entity, c T) bool { return false }
`,
	}
	out, stderr, err := runInFixture(t, files, []string{"./ecs"})
	if err != nil {
		t.Fatalf("Run: %v\nstderr:\n%s", err, stderr)
	}
	if strings.Contains(stderr, RuleUnexportedSig) {
		t.Errorf("unexpected unexported-sig diagnostic; alias should be treated as reachable\nstderr:\n%s", stderr)
	}
	if !strings.Contains(out, "func AddComponent") {
		t.Errorf("wrapper missing from umbrella:\n%s", out)
	}

	cmd := exec.Command("go", "build", "./...")
	cmd.Env = append(os.Environ(), "GOWORK=off")
	if buildOut, gerr := cmd.CombinedOutput(); gerr != nil {
		t.Fatalf("go build failed: %v\n%s", gerr, buildOut)
	}
}

func TestPipeline_Exclude_NoMatch_Warns(t *testing.T) {
	dir := t.TempDir()
	mustWrite := func(rel, content string) {
		t.Helper()
		abs := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite("go.mod", "module testmod\n\ngo 1.24\n")
	mustWrite("pkg/producer/p.go", `package producer

//microwave:export
type Foo struct{}
`)
	// backends/sdl is a sub-package of backends/; there is no Go
	// package directly at backends/. --exclude=./backends therefore
	// matches nothing and microwave should warn.
	mustWrite("backends/sdl/s.go", "package sdl\n")

	old, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })

	var stderr bytes.Buffer
	err := Run(Config{
		Paths:    []string{"./pkg/..."},
		Excludes: []string{"./backends"},
		Out:      "umbrella.go",
		Pkg:      "umbrella",
		Args:     []string{"./pkg/...", "--exclude", "./backends", "--out", "umbrella.go", "--pkg", "umbrella"},
		Stderr:   &stderr,
	})
	if err != nil {
		t.Fatalf("Run: %v\nstderr:\n%s", err, stderr.String())
	}
	out := stderr.String()
	if !strings.Contains(out, RuleExcludeNoMatch) {
		t.Errorf("expected %s warning, got:\n%s", RuleExcludeNoMatch, out)
	}
	if !strings.Contains(out, "./backends/...") {
		t.Errorf("expected the warning to suggest ./backends/...; got:\n%s", out)
	}
}

func TestPipeline_OutOutsideModule_Errors(t *testing.T) {
	files := map[string]string{
		"api/a/a.go": `package a

//microwave:export
type Foo struct{}
`,
	}
	// runInFixture writes go.mod and chdirs into the fixture. To exercise
	// the outside-module check, drive Run directly with an Out outside.
	dir := t.TempDir()
	for rel, content := range files {
		abs := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module testmod\n\ngo 1.24\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	old, _ := os.Getwd()
	_ = os.Chdir(dir)
	t.Cleanup(func() { _ = os.Chdir(old) })

	outsideDir := t.TempDir()
	var buf bytes.Buffer
	err := Run(Config{
		Paths:  []string{"./api/..."},
		Out:    filepath.Join(outsideDir, "umbrella.go"),
		Pkg:    "umbrella",
		Args:   []string{"./api/...", "--out", "x", "--pkg", "umbrella"},
		Stderr: &buf,
	})
	if err == nil {
		t.Fatal("expected ErrUsage, got nil")
	}
	_ = io.Discard
}
