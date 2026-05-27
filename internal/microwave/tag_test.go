package microwave

import (
	"go/ast"
	"testing"
)

func cg(lines ...string) *ast.CommentGroup {
	list := make([]*ast.Comment, 0, len(lines))
	for _, l := range lines {
		list = append(list, &ast.Comment{Text: l})
	}
	return &ast.CommentGroup{List: list}
}

func TestExtractTag(t *testing.T) {
	cases := []struct {
		name      string
		group     *ast.CommentGroup
		wantFound bool
		wantBad   bool
		wantMulti bool
		wantRen   string
		wantDoc   string
	}{
		{
			name:  "nil group",
			group: nil,
		},
		{
			name:  "empty group",
			group: &ast.CommentGroup{},
		},
		{
			name:  "no tag",
			group: cg("// not a tag", "// more text"),
		},
		{
			name:      "tag only",
			group:     cg("//microwave:export"),
			wantFound: true,
		},
		{
			name:      "tag first then doc (pre-gofmt)",
			group:     cg("//microwave:export", "// Foo does the thing."),
			wantFound: true,
			wantDoc:   "// Foo does the thing.",
		},
		{
			name:      "doc then separator then tag (gofmt-normalised)",
			group:     cg("// Foo does the thing.", "//", "//microwave:export"),
			wantFound: true,
			wantDoc:   "// Foo does the thing.",
		},
		{
			name:      "multi-line doc then separator then tag",
			group:     cg("// Foo does X.", "// Second line.", "//", "//microwave:export"),
			wantFound: true,
			wantDoc:   "// Foo does X.\n// Second line.",
		},
		{
			name:      "rename token, gofmt-normalised",
			group:     cg("// Foo.", "//", "//microwave:export Bar"),
			wantFound: true,
			wantRen:   "Bar",
			wantDoc:   "// Foo.",
		},
		{
			name:      "rename token, tag first",
			group:     cg("//microwave:export Bar", "// Foo."),
			wantFound: true,
			wantRen:   "Bar",
			wantDoc:   "// Foo.",
		},
		{
			name:    "malformed extra tokens",
			group:   cg("//microwave:export Bar baz"),
			wantBad: true,
		},
		{
			name:      "trailing whitespace tolerated",
			group:     cg("//microwave:export   "),
			wantFound: true,
		},
		{
			name:      "multi-line doc preserved (tag first)",
			group:     cg("//microwave:export", "// Line 1.", "// Line 2."),
			wantFound: true,
			wantDoc:   "// Line 1.\n// Line 2.",
		},
		{
			name:      "tag with no separator before it",
			group:     cg("// Foo does X.", "//microwave:export"),
			wantFound: true,
			wantDoc:   "// Foo does X.",
		},
		{
			name:      "tag with separator both sides",
			group:     cg("// Above.", "//", "//microwave:export", "//", "// Below."),
			wantFound: true,
			wantDoc:   "// Above.\n// Below.",
		},
		{
			name:      "two tags in one group",
			group:     cg("//microwave:export", "// Foo.", "//microwave:export Bar"),
			wantMulti: true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ren, doc, found, bad, multi := extractTag(c.group)
			if found != c.wantFound {
				t.Fatalf("found = %v, want %v", found, c.wantFound)
			}
			if bad != c.wantBad {
				t.Fatalf("malformed = %v, want %v", bad, c.wantBad)
			}
			if multi != c.wantMulti {
				t.Fatalf("multi = %v, want %v", multi, c.wantMulti)
			}
			if ren != c.wantRen {
				t.Errorf("rename = %q, want %q", ren, c.wantRen)
			}
			if doc != c.wantDoc {
				t.Errorf("doc = %q, want %q", doc, c.wantDoc)
			}
		})
	}
}

func TestHasTagAnywhere(t *testing.T) {
	if !hasTagAnywhere(cg("//microwave:export")) {
		t.Error("expected true for plain tag at line 0")
	}
	if !hasTagAnywhere(cg("// doc", "//", "//microwave:export")) {
		t.Error("expected true for tag at last line (gofmt position)")
	}
	if !hasTagAnywhere(cg("//microwave:export Foo extra")) {
		t.Error("expected true for malformed-but-prefixed line")
	}
	if hasTagAnywhere(cg("// microwave:export")) {
		t.Error("expected false: space after //")
	}
	if hasTagAnywhere(nil) {
		t.Error("expected false for nil")
	}
}

func TestIsBlankComment(t *testing.T) {
	cases := map[string]bool{
		"//":     true,
		"// ":    true,
		"//\t":   true,
		"//  ":   true,
		"// x":   false,
		"//abc":  false,
		"// // ": false,
	}
	for in, want := range cases {
		if got := isBlankComment(in); got != want {
			t.Errorf("isBlankComment(%q) = %v, want %v", in, got, want)
		}
	}
}
