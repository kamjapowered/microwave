package microwave

import (
	"fmt"
	"io"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// Row is one labeled line inside a PrintBox.
type Row struct {
	Label string
	Value string
}

// styles holds the rendered lipgloss styles for one writer. lipgloss
// auto-detects whether the writer is a TTY and falls back to plain
// text when it isn't, so the same code paths work piped or in CI.
type styles struct {
	errSev  lipgloss.Style
	warnSev lipgloss.Style
	arrow   lipgloss.Style
	bullet  lipgloss.Style
	key     lipgloss.Style
	loc     lipgloss.Style
	rule    lipgloss.Style
	muted   lipgloss.Style
	title   lipgloss.Style
	label   lipgloss.Style
	value   lipgloss.Style
	border  lipgloss.Style
}

func newStyles(w io.Writer) styles {
	r := lipgloss.NewRenderer(w)
	return styles{
		errSev:  r.NewStyle().Bold(true).Foreground(lipgloss.Color("9")),  // red
		warnSev: r.NewStyle().Bold(true).Foreground(lipgloss.Color("11")), // yellow
		arrow:   r.NewStyle().Foreground(lipgloss.Color("8")),             // muted grey
		bullet:  r.NewStyle().Foreground(lipgloss.Color("8")),             // muted grey
		key:     r.NewStyle().Foreground(lipgloss.Color("14")),            // cyan
		loc:     r.NewStyle().Foreground(lipgloss.Color("14")),            // cyan
		rule:    r.NewStyle().Faint(true),                                 // faint
		muted:   r.NewStyle().Foreground(lipgloss.Color("8")),             // muted grey
		title:   r.NewStyle().Bold(true).Foreground(lipgloss.Color("10")), // green
		label:   r.NewStyle().Foreground(lipgloss.Color("14")),            // cyan
		value:   r.NewStyle().Bold(true).Foreground(lipgloss.Color("15")), // bright white
		border:  r.NewStyle().Foreground(lipgloss.Color("8")),             // muted grey
	}
}

// PrintError writes a styled error line to w. Use for usage/CLI
// errors that don't carry a source location or any structure.
func PrintError(w io.Writer, msg string) {
	s := newStyles(w)
	fmt.Fprintf(w, "%s %s %s\n",
		s.arrow.Render("→"),
		s.errSev.Render("error:"),
		msg)
}

// PrintWarning writes a styled warning line to w.
func PrintWarning(w io.Writer, msg string) {
	s := newStyles(w)
	fmt.Fprintf(w, "%s %s %s\n",
		s.arrow.Render("→"),
		s.warnSev.Render("warning:"),
		msg)
}

// printDiag writes one Diagnostic in the canonical multi-line form:
//
//	→ error [rule]: summary
//	  |> key1: value1
//	  |> key2: value2
//	  |> at:   /path/to/file.go:line:col
//
// All keys (including the implicit "at:") are right-padded to a common
// width so the values line up.
func printDiag(w io.Writer, s styles, d Diagnostic) {
	var sev string
	switch d.Severity {
	case SevError:
		sev = s.errSev.Render("error")
	case SevWarning:
		sev = s.warnSev.Render("warning")
	}

	rule := ""
	if d.Rule != "" {
		rule = " " + s.rule.Render("["+d.Rule+"]")
	}
	summary := ""
	if d.Summary != "" {
		summary = ": " + d.Summary
	}
	fmt.Fprintf(w, "%s %s%s%s\n",
		s.arrow.Render("→"),
		sev, rule, summary)

	fields := append([]Field{}, d.Fields...)
	loc := formatPos(d.Pos)
	if loc != "<unknown>" {
		fields = append(fields, F("at", loc))
	}

	keyWidth := 0
	for _, f := range fields {
		if n := len(f.Key); n > keyWidth {
			keyWidth = n
		}
	}

	bullet := s.bullet.Render("|>")
	for _, f := range fields {
		key := f.Key + ":"
		pad := keyWidth + 1 - len(f.Key) // room for the ":"
		if pad < 1 {
			pad = 1
		}
		paddedKey := key + spaces(pad-1)

		value := f.Value
		if f.Key == "at" {
			value = s.loc.Render(f.Value)
		}
		fmt.Fprintf(w, "  %s %s %s\n", bullet, s.key.Render(paddedKey), value)
	}
}

func spaces(n int) string {
	if n <= 0 {
		return ""
	}
	out := make([]byte, n)
	for i := range out {
		out[i] = ' '
	}
	return string(out)
}

// PrintBox renders a titled box (tin-style) with key/value rows
// aligned by label width. Rows with an empty Value are skipped.
func PrintBox(w io.Writer, title string, rows []Row) {
	s := newStyles(w)

	visible := rows[:0]
	for _, r := range rows {
		if r.Value != "" {
			visible = append(visible, r)
		}
	}
	if len(visible) == 0 {
		return
	}

	labelWidth := 0
	for _, r := range visible {
		if n := len(r.Label); n > labelWidth {
			labelWidth = n
		}
	}

	bodyLines := make([]string, 0, len(visible))
	contentWidth := 0
	for _, r := range visible {
		label := s.label.Render(r.Label + ":" + spaces(labelWidth-len(r.Label)))
		value := s.value.Render(r.Value)
		line := label + "  " + value
		bodyLines = append(bodyLines, line)
		if width := lipgloss.Width(line); width > contentWidth {
			contentWidth = width
		}
	}

	inner := contentWidth + 2
	titleText := s.title.Render(title)
	dashes := inner - lipgloss.Width(titleText) - 3
	if dashes < 1 {
		dashes = 1
	}
	top := s.border.Render("╭─ ") + titleText + s.border.Render(" "+strings.Repeat("─", dashes)+"╮")
	bottom := s.border.Render("╰" + strings.Repeat("─", inner) + "╯")

	fmt.Fprintln(w)
	fmt.Fprintln(w, top)
	for _, line := range bodyLines {
		pad := contentWidth - lipgloss.Width(line)
		if pad < 0 {
			pad = 0
		}
		fmt.Fprintln(w, s.border.Render("│")+" "+line+spaces(pad)+" "+s.border.Render("│"))
	}
	fmt.Fprintln(w, bottom)
}
