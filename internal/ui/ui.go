// Package ui formats what restorable found for a terminal: sizes, ages, paths and
// simple tables, with colour only when a terminal is there to read it.
package ui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// Colour reports whether styled output is wanted: a terminal, no NO_COLOR, and
// not switched off for a screenshot or a test.
func Colour(f *os.File) bool {
	if os.Getenv("NO_COLOR") != "" || os.Getenv("RESTORABLE_NO_COLOR") == "1" {
		return false
	}
	if os.Getenv("RESTORABLE_FORCE_COLOR") == "1" {
		return true
	}
	_, err := unix.IoctlGetTermios(int(f.Fd()), unix.TIOCGETA)
	return err == nil
}

// Style is a set of escape codes, empty when colour is off.
type Style struct{ on bool }

// NewStyle makes a style set for a file.
func NewStyle(f *os.File) Style { return Style{on: Colour(f)} }

func (s Style) wrap(code, text string) string {
	if !s.on || text == "" {
		return text
	}
	return "\x1b[" + code + "m" + text + "\x1b[0m"
}

func (s Style) Bold(t string) string   { return s.wrap("1", t) }
func (s Style) Dim(t string) string    { return s.wrap("2", t) }
func (s Style) Red(t string) string    { return s.wrap("31", t) }
func (s Style) Green(t string) string  { return s.wrap("32", t) }
func (s Style) Yellow(t string) string { return s.wrap("33", t) }
func (s Style) Blue(t string) string   { return s.wrap("34", t) }

// Bytes formats a size the way a person reads it.
func Bytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	value := float64(n)
	units := []string{"KB", "MB", "GB", "TB", "PB"}
	for _, u := range units {
		value /= unit
		if value < unit {
			if value < 10 {
				return fmt.Sprintf("%.1f %s", value, u)
			}
			return fmt.Sprintf("%.0f %s", value, u)
		}
	}
	return fmt.Sprintf("%.0f EB", value/unit)
}

// Count formats a file count with thousands separators.
func Count(n int) string {
	s := fmt.Sprintf("%d", n)
	if len(s) <= 3 {
		return s
	}
	var out []byte
	for i, c := range []byte(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ' ')
		}
		out = append(out, c)
	}
	return string(out)
}

// Age says how long ago something happened, in words.
func Age(d time.Duration) string {
	switch {
	case d < 0:
		return "in the future"
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%d min ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d h ago", int(d.Hours()))
	case d < 60*24*time.Hour:
		return fmt.Sprintf("%d days ago", int(d.Hours()/24))
	default:
		return fmt.Sprintf("%d months ago", int(d.Hours()/24/30))
	}
}

// Duration writes a duration the way a person says it: 48h, 90 min, 36 h.
func Duration(d time.Duration) string {
	switch {
	case d <= 0:
		return "0"
	case d < time.Minute:
		return fmt.Sprintf("%d s", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%d min", int(d.Minutes()))
	case d%(24*time.Hour) == 0 && d >= 24*time.Hour:
		return fmt.Sprintf("%d days", int(d.Hours()/24))
	case d%time.Hour == 0:
		return fmt.Sprintf("%d h", int(d.Hours()))
	default:
		return fmt.Sprintf("%.1f h", d.Hours())
	}
}

// Path shortens a path for a terminal: home becomes ~, and a very long path
// loses its middle rather than its end, because the end is the useful part.
func Path(p string, width int) string {
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		if p == home {
			p = "~"
		} else if strings.HasPrefix(p, home+string(filepath.Separator)) {
			p = "~" + p[len(home):]
		}
	}
	if width <= 0 || len([]rune(p)) <= width {
		return p
	}
	r := []rune(p)
	keep := width - 1
	head := keep / 3
	tail := keep - head
	return string(r[:head]) + "…" + string(r[len(r)-tail:])
}

// Table lays out rows in aligned columns.
type Table struct {
	Head  []string
	Rows  [][]string
	Align []bool // true = right
}

// String renders the table.
func (t Table) String() string {
	cols := len(t.Head)
	for _, r := range t.Rows {
		if len(r) > cols {
			cols = len(r)
		}
	}
	width := make([]int, cols)
	measure := func(row []string) {
		for i, c := range row {
			if w := visibleWidth(c); w > width[i] {
				width[i] = w
			}
		}
	}
	if len(t.Head) > 0 {
		measure(t.Head)
	}
	for _, r := range t.Rows {
		measure(r)
	}
	var b strings.Builder
	write := func(row []string) {
		for i, c := range row {
			pad := width[i] - visibleWidth(c)
			right := i < len(t.Align) && t.Align[i]
			if right {
				b.WriteString(strings.Repeat(" ", pad))
			}
			b.WriteString(c)
			if i < len(row)-1 {
				if !right {
					b.WriteString(strings.Repeat(" ", pad))
				}
				b.WriteString("  ")
			}
		}
		b.WriteString("\n")
	}
	if len(t.Head) > 0 {
		write(t.Head)
	}
	for _, r := range t.Rows {
		write(r)
	}
	return b.String()
}

// visibleWidth counts runes, ignoring escape sequences.
func visibleWidth(s string) int {
	n, esc := 0, false
	for _, r := range s {
		switch {
		case esc:
			if r == 'm' {
				esc = false
			}
		case r == '\x1b':
			esc = true
		default:
			n++
		}
	}
	return n
}

// TerminalWidth is the width of the terminal, or 100 when there is none.
func TerminalWidth(f *os.File) int {
	if v := os.Getenv("RESTORABLE_WIDTH"); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n > 20 {
			return n
		}
	}
	ws, err := unix.IoctlGetWinsize(int(f.Fd()), unix.TIOCGWINSZ)
	if err != nil || ws.Col < 20 {
		return 100
	}
	return int(ws.Col)
}
