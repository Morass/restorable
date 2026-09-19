package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/morass/restorable/internal/app"
	"github.com/morass/restorable/internal/backend"
	"github.com/morass/restorable/internal/config"
	"github.com/morass/restorable/internal/coverage"
	"github.com/morass/restorable/internal/ui"
)

func cmdCoverage(ctx context.Context, a *app.App, args []string) (int, error) {
	fs := newFlagSet("coverage")
	var (
		asJSON = fs.Bool("json", false, "print JSON")
		depth  = fs.Int("depth", coverage.DefaultMaxDepth, "how deep to look inside protected trees")
		minSz  = fs.String("min-size", "1MB", "ignore holes smaller than this")
		files  = fs.Bool("files", false, "ask about individual files too")
		all    = fs.Bool("all", false, "include folders macOS guards behind a permission prompt")
		cross  = fs.Bool("cross-filesystems", false, "walk onto other volumes")
		strict = fs.Bool("strict", false, "exit 1 when unprotected data was found")
		quiet  = fs.Bool("quiet", false, "no progress line")
	)
	if code, done := parse(fs, args); done {
		return code, nil
	}
	min, err := parseSize(*minSz)
	if err != nil {
		return exitUsage, err
	}
	// --min-size keeps a terminal table readable. JSON is read by a program, so
	// unless the size was asked for explicitly every hole is listed.
	if *asJSON && !flagGiven(fs, "min-size") {
		min = 0
	}
	opts := coverage.Options{
		MaxDepth: *depth, MinBytes: min, CheckFiles: *files,
		AllLocations: *all, CrossFilesystems: *cross,
	}
	for _, r := range fs.Args() {
		p, err := config.ExpandPath(r)
		if err != nil {
			return exitUsage, err
		}
		opts.Roots = append(opts.Roots, p)
	}

	progress := newProgress(!*quiet && !*asJSON)
	opts.Progress = progress.dir
	rep, err := a.Coverage(ctx, opts)
	progress.done()
	if err != nil {
		return exitError, err
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rep); err != nil {
			return exitError, err
		}
	} else {
		printCoverage(rep)
	}
	if *strict && rep.UnprotectedFiles > 0 {
		return exitProblem, nil
	}
	return exitOK, nil
}

func printCoverage(rep coverage.Report) {
	s := ui.NewStyle(os.Stdout)
	width := ui.TerminalWidth(os.Stdout)

	fmt.Println(s.Bold("Backups seen"))
	if len(rep.Destinations) == 0 {
		fmt.Println("  " + s.Red("none") + " — nothing on this machine could be read as a backup")
	}
	for _, d := range rep.Destinations {
		fmt.Printf("  %s\n", destLine(s, d, width))
	}

	unprotected := rep.Unprotected()
	fmt.Println()
	fmt.Println(s.Bold("Coverage"))
	share := 0.0
	if rep.TotalBytes > 0 {
		share = float64(rep.UnprotectedBytes) / float64(rep.TotalBytes) * 100
	}
	fmt.Printf("  walked %s in %s files across %s\n",
		ui.Bytes(rep.TotalBytes), ui.Count(rep.TotalFiles), strings.Join(shorten(rep.Roots, width/2), ", "))
	switch {
	case rep.UnprotectedFiles == 0 && rep.TotalFiles > 0:
		fmt.Printf("  %s every file walked is claimed by a backup\n", s.Green("✓"))
	case rep.TotalFiles == 0:
		fmt.Printf("  %s nothing was walked\n", s.Yellow("!"))
	default:
		fmt.Printf("  %s %s in %s files is protected by nothing (%.1f%% of what was walked)\n",
			s.Red("✗"), ui.Bytes(rep.UnprotectedBytes), ui.Count(rep.UnprotectedFiles), share)
	}

	if len(unprotected) > 0 {
		fmt.Println()
		fmt.Println(s.Bold("The largest holes"))
		t := ui.Table{Head: []string{s.Dim("SIZE"), s.Dim("FILES"), s.Dim("PATH"), s.Dim("WHY")},
			Align: []bool{true, true, false, false}}
		pathW := max(24, width-58)
		for i, f := range unprotected {
			if i >= 15 {
				fmt.Printf("  %s\n", s.Dim(fmt.Sprintf("… and %d more", len(unprotected)-15)))
				break
			}
			t.Rows = append(t.Rows, []string{
				ui.Bytes(f.Bytes), ui.Count(f.Files), ui.Path(f.Path, pathW), reasonText(s, f),
			})
		}
		fmt.Print(indent(t.String(), "  "))
	}

	var skipped, unreadable []coverage.Finding
	for _, f := range rep.Findings {
		switch f.Kind {
		case coverage.Skipped:
			skipped = append(skipped, f)
		case coverage.Unreadable:
			unreadable = append(unreadable, f)
		}
	}
	if len(unreadable) > 0 {
		fmt.Println()
		fmt.Printf("%s\n", s.Bold("Could not look"))
		for i, f := range unreadable {
			if i >= 5 {
				fmt.Printf("  %s\n", s.Dim(fmt.Sprintf("… and %d more", len(unreadable)-5)))
				break
			}
			fmt.Printf("  %s — %s\n", ui.Path(f.Path, width-40), s.Dim(ui.Safe(f.Detail)))
		}
	}
	if len(skipped) > 0 {
		fmt.Println()
		fmt.Printf("%s %s\n", s.Dim("Skipped on purpose:"), s.Dim(fmt.Sprintf("%d folders (--all includes them)", len(skipped))))
	}
	if !rep.CheckedFiles {
		fmt.Println()
		fmt.Println(s.Dim("Directories were asked about, not single files. --files checks those too."))
	}
}

func reasonText(s ui.Style, f coverage.Finding) string {
	switch f.Kind {
	case coverage.NoBackup:
		// The usual reason is the tag itself; only an unusual one is worth words.
		if f.Detail == "" || f.Detail == "no backup destination covers this path" {
			return s.Red("no backup keeps it")
		}
		return s.Red("no backup") + " — " + ui.Safe(f.Detail)
	case coverage.Excluded:
		return s.Yellow("excluded") + " — " + ui.Safe(f.Detail)
	default:
		return f.Detail
	}
}

func destLine(s ui.Style, d backend.Destination, width int) string {
	label := d.Label
	if label == "" {
		label = d.ID
	}
	if label == "" {
		label = string(d.Backend)
	}
	head := fmt.Sprintf("%-12s %s", d.Backend, ui.Path(label, max(20, width/3)))
	switch d.State {
	case backend.StateOK:
		when := "never completed"
		if !d.Connected {
			if !d.LastOK.IsZero() {
				return head + "  " + s.Yellow("not connected") + "  " +
					s.Dim("last backup "+ui.Age(time.Since(d.LastOK))+" ("+d.LastOKSource+")")
			}
			return head + "  " + s.Yellow("not connected") + "  " + s.Dim("nothing is being kept to it right now")
		}
		if !d.LastOK.IsZero() {
			when = ui.Age(time.Since(d.LastOK))
			if d.LastOKSource != "" {
				when += " (" + d.LastOKSource + ")"
			}
		}
		return head + "  " + s.Green("readable") + "  " + s.Dim(when)
	case backend.StateLocked:
		return head + "  " + s.Yellow("locked") + "  " + s.Dim(ui.Safe(d.Err))
	case backend.StateUnreachable:
		return head + "  " + s.Red("not there") + "  " + s.Dim(ui.Safe(d.Err))
	default:
		return head + "  " + s.Red("error") + "  " + s.Dim(ui.Safe(d.Err))
	}
}

func shorten(paths []string, width int) []string {
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		out = append(out, ui.Path(p, width))
	}
	return out
}

func indent(s, with string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		lines[i] = with + l
	}
	return strings.Join(lines, "\n") + "\n"
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// flagGiven reports whether a flag was set on the command line.
func flagGiven(fs *flag.FlagSet, name string) bool {
	found := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			found = true
		}
	})
	return found
}

// parse reads the flags and says whether the command should stop: asking for
// help is not a usage error, and a bad flag has already been reported.
func parse(fs *flag.FlagSet, args []string) (int, bool) {
	err := fs.Parse(args)
	switch {
	case err == nil:
		return exitOK, false
	case errors.Is(err, flag.ErrHelp):
		return exitOK, true
	default:
		return exitUsage, true
	}
}

func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.Usage = func() {
		if t, ok := topics[name]; ok {
			fmt.Fprintln(os.Stderr, t.body)
			return
		}
		usage(os.Stderr)
	}
	return fs
}

// parseSize reads 100, 100KB, 32MB, 2GB.
func parseSize(s string) (int64, error) {
	s = strings.TrimSpace(strings.ToUpper(s))
	if s == "" {
		return 0, nil
	}
	mult := int64(1)
	switch {
	case strings.HasSuffix(s, "KB"), strings.HasSuffix(s, "K"):
		mult, s = 1<<10, strings.TrimSuffix(strings.TrimSuffix(s, "KB"), "K")
	case strings.HasSuffix(s, "MB"), strings.HasSuffix(s, "M"):
		mult, s = 1<<20, strings.TrimSuffix(strings.TrimSuffix(s, "MB"), "M")
	case strings.HasSuffix(s, "GB"), strings.HasSuffix(s, "G"):
		mult, s = 1<<30, strings.TrimSuffix(strings.TrimSuffix(s, "GB"), "G")
	case strings.HasSuffix(s, "TB"), strings.HasSuffix(s, "T"):
		mult, s = 1<<40, strings.TrimSuffix(strings.TrimSuffix(s, "TB"), "T")
	case strings.HasSuffix(s, "B"):
		s = strings.TrimSuffix(s, "B")
	}
	var f float64
	if _, err := fmt.Sscanf(strings.TrimSpace(s), "%g", &f); err != nil {
		return 0, fmt.Errorf("%q is not a size (try 100MB)", s)
	}
	if f < 0 {
		return 0, fmt.Errorf("a size cannot be negative")
	}
	return int64(f * float64(mult)), nil
}

// progress prints a single rewritten line while a walk runs.
type progress struct {
	on    bool
	last  time.Time
	width int
}

func newProgress(on bool) *progress {
	return &progress{on: on && ui.Colour(os.Stderr), width: ui.TerminalWidth(os.Stderr)}
}

func (p *progress) dir(path string) {
	if !p.on || time.Since(p.last) < 100*time.Millisecond {
		return
	}
	p.last = time.Now()
	fmt.Fprintf(os.Stderr, "\r\x1b[2K  looking at %s", ui.Path(path, p.width-16))
}

func (p *progress) done() {
	if p.on {
		fmt.Fprint(os.Stderr, "\r\x1b[2K")
	}
}
