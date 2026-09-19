package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/morass/restorable/internal/config"

	"github.com/morass/restorable/internal/app"
	"github.com/morass/restorable/internal/drill"
	"github.com/morass/restorable/internal/ui"
)

func cmdDrill(ctx context.Context, a *app.App, args []string) (int, error) {
	fs := newFlagSet("drill")
	var (
		dest    = fs.String("dest", "", "which destination to drill")
		count   = fs.Int("count", drill.DefaultCount, "how many files to sample")
		seed    = fs.Int64("seed", 0, "sample the same files again")
		maxSize = fs.String("max-size", "32MB", "skip files larger than this")
		target  = fs.String("target", "", "restore into this directory")
		keep    = fs.Bool("keep", false, "keep the restored copies")
		asJSON  = fs.Bool("json", false, "print JSON")
		path    = fs.String("path", "", "sample from this directory of the snapshot (default: your home directory)")
	)
	if code, done := parse(fs, args); done {
		return code, nil
	}
	maxBytes, err := parseSize(*maxSize)
	if err != nil {
		return exitUsage, err
	}
	if *count <= 0 {
		return exitUsage, fmt.Errorf("--count must be at least 1")
	}

	scope := *path
	if scope == "" {
		// A backup holds the whole machine; a drill is about the user's own files.
		if home, err := os.UserHomeDir(); err == nil {
			scope = home
		}
	} else {
		expanded, err := config.ExpandPath(scope)
		if err != nil {
			return exitUsage, err
		}
		scope = expanded
	}

	pair, snap, err := a.Drillable(ctx, *dest)
	if err != nil {
		return exitProblem, err
	}
	s := ui.NewStyle(os.Stdout)
	if !*asJSON {
		where := pair.Dest.Label
		if where == "" {
			where = pair.Dest.ID
		}
		fmt.Printf("%s %s %s %s\n", s.Bold("Drilling"), ui.Safe(where),
			s.Dim("snapshot"), fmt.Sprintf("%s (%s)", ui.Safe(snap.ID), ui.Age(time.Since(snap.Time))))
		if scope != "" {
			fmt.Printf("  %s\n", s.Dim("sampling files under "+ui.Path(scope, 60)))
		}
	}
	opts := drill.Options{Count: *count, Seed: *seed, MaxBytes: maxBytes, Target: *target, Keep: *keep, Path: scope}
	if !*asJSON {
		opts.Progress = func(msg string) { fmt.Printf("  %s\n", s.Dim(msg)) }
	}
	askedSeed := *seed != 0
	rec, receiptPath, err := a.Drill(ctx, pair, snap, opts)
	if err != nil {
		return exitError, err
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rec); err != nil {
			return exitError, err
		}
	} else {
		printDrill(rec, receiptPath, askedSeed)
	}
	if !rec.Pass {
		return exitProblem, nil
	}
	return exitOK, nil
}

func printDrill(rec drill.Receipt, receiptPath string, askedSeed bool) {
	s := ui.NewStyle(os.Stdout)
	width := ui.TerminalWidth(os.Stdout)
	fmt.Println()
	t := ui.Table{Head: []string{s.Dim("RESULT"), s.Dim("SIZE"), s.Dim("FILE")}, Align: []bool{false, true, false}}
	for _, f := range rec.Files {
		t.Rows = append(t.Rows, []string{statusText(s, f.Status), ui.Bytes(f.Bytes), ui.Path(f.Path, max(24, width-40))})
	}
	fmt.Print(indent(t.String(), "  "))

	counts := rec.Counts()
	fmt.Println()
	if rec.Pass {
		fmt.Printf("  %s %d of %d files came back byte for byte",
			s.Green("✓"), counts[drill.Match], len(rec.Files))
	} else {
		fmt.Printf("  %s the drill found a problem: %d of %d files did not come back as they went in",
			s.Red("✗"), badCount(rec), len(rec.Files))
	}
	var extra []string
	for _, st := range []drill.Status{drill.ChangedSince, drill.GoneLive, drill.Differs, drill.NotRestored, drill.Failed} {
		if counts[st] > 0 {
			extra = append(extra, fmt.Sprintf("%d %s", counts[st], st))
		}
	}
	if len(extra) > 0 {
		fmt.Printf(" %s", s.Dim("("+join(extra, ", ")+")"))
	}
	fmt.Println()
	for _, f := range rec.Files {
		if f.Status.Bad() && f.Detail != "" {
			fmt.Printf("    %s %s: %s\n", s.Red("✗"), ui.Path(f.Path, max(20, width-50)), ui.Safe(f.Detail))
		}
	}
	if rec.Note != "" {
		fmt.Printf("  %s\n", s.Dim(ui.Safe(rec.Note)))
	}
	sampled := fmt.Sprintf("%d files sampled in %s", len(rec.Files), app.Round(rec.Duration))
	if askedSeed {
		sampled = fmt.Sprintf("%d files sampled with seed %d in %s", len(rec.Files), rec.Seed, app.Round(rec.Duration))
	}
	fmt.Printf("  %s\n", s.Dim(sampled))
	if receiptPath != "" {
		fmt.Printf("  %s\n", s.Dim("receipt: "+ui.Path(receiptPath, width-12)))
	}
}

func badCount(rec drill.Receipt) int {
	n := 0
	for _, f := range rec.Files {
		if f.Status.Bad() {
			n++
		}
	}
	return n
}

func statusText(s ui.Style, st drill.Status) string {
	switch st {
	case drill.Match:
		return s.Green("match")
	case drill.ChangedSince:
		return s.Blue("changed")
	case drill.GoneLive:
		return s.Dim("only in backup")
	case drill.Differs:
		return s.Red("DIFFERS")
	case drill.NotRestored:
		return s.Red("NOT RESTORED")
	default:
		return s.Red("failed")
	}
}

func join(items []string, sep string) string {
	out := ""
	for i, it := range items {
		if i > 0 {
			out += sep
		}
		out += it
	}
	return out
}
