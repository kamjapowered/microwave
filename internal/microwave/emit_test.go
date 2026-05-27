package microwave

import "testing"

func TestAssignAliases_Deterministic(t *testing.T) {
	in := map[string]bool{
		"github.com/acme/foo/api/a":  true,
		"github.com/acme/foo/api/b":  true,
		"github.com/acme/bar/api/a":  true, // collides on last segment "a"
		"example.com/some/internal": true,  // collides on "internal" with hypothetical other
	}
	out := assignAliases(in)
	if got := out["github.com/acme/bar/api/a"]; got != "a" {
		t.Errorf("expected first sorted 'a' path to win alias 'a'; got %q for bar", got)
	}
	if got := out["github.com/acme/foo/api/a"]; got != "a2" {
		t.Errorf("expected second 'a' path to get 'a2'; got %q for foo", got)
	}
	if got := out["github.com/acme/foo/api/b"]; got != "b" {
		t.Errorf("expected 'b' alias; got %q", got)
	}

	// Determinism: running again gives same map.
	out2 := assignAliases(in)
	for k, v := range out {
		if out2[k] != v {
			t.Errorf("non-deterministic: %s = %q vs %q", k, v, out2[k])
		}
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
