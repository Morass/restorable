package ui_test

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/morass/restorable/internal/ui"
)

func TestBytesReadsLikeAPerson(t *testing.T) {
	cases := map[int64]string{
		0: "0 B", 512: "512 B", 1024: "1.0 KB", 1536: "1.5 KB",
		20 * 1024: "20 KB", 1024 * 1024: "1.0 MB",
		3*1024*1024*1024 + 512*1024*1024: "3.5 GB",
		2 << 40:                          "2.0 TB",
	}
	for in, want := range cases {
		if got := ui.Bytes(in); got != want {
			t.Errorf("Bytes(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestCountGroupsThousands(t *testing.T) {
	for in, want := range map[int]string{7: "7", 999: "999", 1000: "1 000", 1234567: "1 234 567"} {
		if got := ui.Count(in); got != want {
			t.Errorf("Count(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestAgeIsInWords(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{10 * time.Second, "just now"},
		{20 * time.Minute, "20 min ago"},
		{5 * time.Hour, "5 h ago"},
		{50 * time.Hour, "2 days ago"},
		{70 * 24 * time.Hour, "2 months ago"},
		{-time.Hour, "in the future"},
	}
	for _, c := range cases {
		if got := ui.Age(c.d); got != c.want {
			t.Errorf("Age(%s) = %q, want %q", c.d, got, c.want)
		}
	}
}

func TestDurationIsReadable(t *testing.T) {
	for in, want := range map[time.Duration]string{
		48 * time.Hour: "2 days", 36 * time.Hour: "36 h",
		90 * time.Minute: "1.5 h", 30 * time.Minute: "30 min",
	} {
		if got := ui.Duration(in); got != want {
			t.Errorf("Duration(%s) = %q, want %q", in, got, want)
		}
	}
}

func TestPathShortensTheMiddleAndUsesTilde(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}
	if got := ui.Path(home+"/Documents/a.txt", 0); got != "~/Documents/a.txt" {
		t.Errorf("Path = %q, want the home directory as ~", got)
	}
	long := "/very/long/path/that/keeps/going/and/going/file-with-a-name.txt"
	got := ui.Path(long, 30)
	if len([]rune(got)) != 30 {
		t.Errorf("Path = %q (%d runes), want exactly 30", got, len([]rune(got)))
	}
	if !strings.HasSuffix(got, "file-with-a-name.txt") {
		t.Errorf("Path = %q, want the end kept: that is the useful part", got)
	}
	if !strings.Contains(got, "…") {
		t.Errorf("Path = %q, want an ellipsis where it was cut", got)
	}
}

func TestTableAlignsAroundColourCodes(t *testing.T) {
	t.Setenv("RESTORABLE_FORCE_COLOR", "1")
	s := ui.NewStyle(os.Stdout)
	tbl := ui.Table{
		Head:  []string{"SIZE", "PATH"},
		Align: []bool{true, false},
		Rows: [][]string{
			{"1.0 KB", s.Red("short")},
			{"12 GB", "a-much-longer-path"},
		},
	}
	lines := strings.Split(strings.TrimRight(tbl.String(), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("lines = %d, want head plus two rows", len(lines))
	}
	// The visible column must line up even though one cell holds escape codes.
	col := strings.Index(lines[1], "\x1b")
	if col < 0 {
		t.Fatal("the coloured cell lost its escape codes")
	}
	plain := stripANSI(lines[1])
	if !strings.HasPrefix(plain, "1.0 KB") {
		t.Errorf("row = %q, want the size right-aligned first", plain)
	}
	if strings.Index(plain, "short") != strings.Index(stripANSI(lines[2]), "a-much-longer-path") {
		t.Errorf("columns do not line up:\n%q\n%q", stripANSI(lines[1]), stripANSI(lines[2]))
	}
}

func stripANSI(s string) string {
	var b strings.Builder
	esc := false
	for _, r := range s {
		switch {
		case esc:
			if r == 'm' {
				esc = false
			}
		case r == '\x1b':
			esc = true
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func TestColourIsOffWhenAsked(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	if ui.Colour(os.Stdout) {
		t.Error("NO_COLOR must switch colour off")
	}
	s := ui.NewStyle(os.Stdout)
	if s.Red("x") != "x" {
		t.Errorf("styled text = %q, want it plain", s.Red("x"))
	}
}

func TestTerminalWidthCanBeFixedForScreenshots(t *testing.T) {
	t.Setenv("RESTORABLE_WIDTH", "72")
	if got := ui.TerminalWidth(os.Stdout); got != 72 {
		t.Errorf("width = %d, want the fixed 72", got)
	}
}

func TestAFileNameCannotRepaintTheTerminal(t *testing.T) {
	// A file called "notes\x1b[2J\x1b[H(nothing here)" would clear the screen if
	// its name were printed as it is.
	hostile := "/tmp/notes\x1b[2J\x1b[Hgone\r\x07"
	got := ui.Safe(hostile)
	for _, bad := range []string{"\x1b", "\r", "\x07"} {
		if strings.Contains(got, bad) {
			t.Errorf("Safe kept %q in %q", bad, got)
		}
	}
	if !strings.Contains(got, "notes") || !strings.Contains(got, "gone") {
		t.Errorf("Safe = %q, want the readable parts kept", got)
	}
	if ui.Safe("/Users/x/Documents/a b.txt") != "/Users/x/Documents/a b.txt" {
		t.Error("an ordinary name must pass through untouched")
	}
	if !strings.Contains(ui.Path(hostile, 0), "\\x1b") {
		t.Errorf("Path must sanitise too: %q", ui.Path(hostile, 0))
	}
}
