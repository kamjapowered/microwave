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
	}
	for _, want := range checks {
		if !strings.Contains(out, want) {
			t.Errorf("umbrella missing %q\nfull output:\n%s", want, out)
		}
	}

	cmd := exec.Command("go", "build", "./...")
	cmd.Env = append(os.Environ(), "GOWORK=off")
	out2, gerr := cmd.CombinedOutput()
	if gerr != nil {
		t.Fatalf("go build on emitted umbrella failed: %v\n%s\n--- umbrella ---\n%s", gerr, out2, out)
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
