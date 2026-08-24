package microwave

import "testing"

func TestAssignAliases_NoCollision_UsesNaturalName(t *testing.T) {
	in := map[string]string{
		"github.com/acme/foo/api/a":  "a",
		"github.com/acme/foo/api/b":  "b",
		"github.com/example/widgets": "widgets",
	}
	out := assignAliases(in)
	for path, want := range map[string]string{
		"github.com/acme/foo/api/a":  "a",
		"github.com/acme/foo/api/b":  "b",
		"github.com/example/widgets": "widgets",
	} {
		if got := out[path]; got != want {
			t.Errorf("path %q: chosen = %q, want %q", path, got, want)
		}
	}
}

func TestAssignAliases_Collision_SuffixesInSortedOrder(t *testing.T) {
	in := map[string]string{
		"github.com/acme/foo/api/a": "a",
		"github.com/acme/bar/api/a": "a", // collides on declared name
	}
	out := assignAliases(in)
	if got := out["github.com/acme/bar/api/a"]; got != "a" {
		t.Errorf("first-sorted path got %q, want \"a\"", got)
	}
	if got := out["github.com/acme/foo/api/a"]; got != "a2" {
		t.Errorf("second-sorted path got %q, want \"a2\"", got)
	}

	// Determinism: a second call yields the same map.
	out2 := assignAliases(in)
	for k, v := range out {
		if out2[k] != v {
			t.Errorf("non-deterministic: %s = %q vs %q", k, v, out2[k])
		}
	}
}

func TestAssignAliases_MissingNaturalName_FallsBackToLastSegment(t *testing.T) {
	in := map[string]string{
		"github.com/acme/foo/api/a": "", // missing declared name
	}
	out := assignAliases(in)
	if got := out["github.com/acme/foo/api/a"]; got != "a" {
		t.Errorf("got %q, want \"a\"", got)
	}
}

func TestLastSegment(t *testing.T) {
	cases := map[string]string{
		"github.com/acme/foo": "foo",
		"foo":                 "foo",
		"":                    "",
		"a/b/c":               "c",
	}
	for in, want := range cases {
		if got := lastSegment(in); got != want {
			t.Errorf("lastSegment(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSanitizeAlias(t *testing.T) {
	cases := map[string]string{
		"foo":     "foo",
		"foo-bar": "foo_bar",
		"3d":      "p3d",
		"":        "pkg",
		"a.b":     "a_b",
	}
	for in, want := range cases {
		if got := sanitizeAlias(in); got != want {
			t.Errorf("sanitizeAlias(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestGoVersionLT(t *testing.T) {
	cases := []struct {
		v, target string
		want      bool
	}{
		{"1.21", "1.24", true},
		{"1.24", "1.24", false},
		{"1.25", "1.24", false},
		{"1.24.1", "1.24", false},
		{"1.23.9", "1.24", true},
	}
	for _, c := range cases {
		if got := goVersionLT(c.v, c.target); got != c.want {
			t.Errorf("goVersionLT(%q, %q) = %v, want %v", c.v, c.target, got, c.want)
		}
	}
}
