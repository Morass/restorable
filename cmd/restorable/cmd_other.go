package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/morass/restorable/internal/app"
	"github.com/morass/restorable/internal/config"
	"github.com/morass/restorable/internal/drill"
	"github.com/morass/restorable/internal/state"
	"github.com/morass/restorable/internal/ui"
)

func cmdHistory(args []string) (int, error) {
	fs := newFlagSet("history")
	limit := fs.Int("limit", 20, "how many receipts to show")
	asJSON := fs.Bool("json", false, "print JSON")
	if err := fs.Parse(args); err != nil {
		return exitUsage, nil
	}
	rs, err := state.Receipts(*limit)
	if err != nil {
		return exitError, err
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rs); err != nil {
			return exitError, err
		}
		return exitOK, nil
	}
	s := ui.NewStyle(os.Stdout)
	if len(rs) == 0 {
		fmt.Println("No drill has run on this machine yet. `restorable drill` writes the first receipt.")
		return exitOK, nil
	}
	width := ui.TerminalWidth(os.Stdout)
	t := ui.Table{
		Head:  []string{s.Dim("WHEN"), s.Dim("RESULT"), s.Dim("BACKUP"), s.Dim("WHERE"), s.Dim("SNAPSHOT"), s.Dim("FILES")},
		Align: []bool{false, false, false, false, false, true},
	}
	for _, r := range rs {
		result := s.Green("pass")
		if !r.Pass {
			result = s.Red("FAIL")
		}
		where := r.Label
		if where == "" {
			where = r.Destination
		}
		counts := r.Counts()
		files := fmt.Sprintf("%d/%d", counts[drill.Match], len(r.Files))
		t.Rows = append(t.Rows, []string{
			r.RanAt.Local().Format("2006-01-02 15:04"), result, string(r.Backend),
			ui.Path(where, max(18, width/4)), r.Snapshot, files,
		})
	}
	fmt.Print(t.String())
	return exitOK, nil
}

func cmdBackends(ctx context.Context, a *app.App, args []string) (int, error) {
	fs := newFlagSet("backends")
	asJSON := fs.Bool("json", false, "print JSON")
	if err := fs.Parse(args); err != nil {
		return exitUsage, nil
	}
	pairs, err := a.Destinations(ctx)
	if err != nil {
		return exitError, err
	}
	if *asJSON {
		var dests []any
		for _, p := range pairs {
			dests = append(dests, p.Dest)
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(dests); err != nil {
			return exitError, err
		}
		return exitOK, nil
	}
	s := ui.NewStyle(os.Stdout)
	width := ui.TerminalWidth(os.Stdout)
	if len(pairs) == 0 {
		fmt.Println("No backup system was found here. restorable reads Time Machine, restic and borg.")
		return exitOK, nil
	}
	for _, p := range pairs {
		fmt.Println(destLine(s, p.Dest, width))
		if len(p.Dest.Roots) > 0 {
			fmt.Printf("  %s %s\n", s.Dim("covers"), join(shorten(p.Dest.Roots, max(20, width/2)), ", "))
		}
		if p.Dest.Snapshots > 0 {
			fmt.Printf("  %s %d\n", s.Dim("snapshots"), p.Dest.Snapshots)
		}
	}
	if cfgPath := a.Config.Path(); cfgPath != "" {
		fmt.Printf("\n%s\n", s.Dim("configuration: "+ui.Path(cfgPath, width-20)))
	} else {
		fmt.Printf("\n%s\n", s.Dim("no configuration file; add repositories with `restorable config --write`"))
	}
	return exitOK, nil
}

func cmdConfig(args []string) (int, error) {
	fs := newFlagSet("config")
	showPath := fs.Bool("path", false, "print the configuration path")
	example := fs.Bool("example", false, "print an example configuration")
	write := fs.Bool("write", false, "write the example configuration if there is none")
	if err := fs.Parse(args); err != nil {
		return exitUsage, nil
	}
	path, err := config.File()
	if err != nil {
		return exitError, err
	}
	switch {
	case *example:
		fmt.Print(config.Example)
	case *write:
		if _, err := os.Stat(path); err == nil {
			return exitProblem, fmt.Errorf("%s already exists; edit it instead", path)
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return exitError, err
		}
		if err := os.WriteFile(path, []byte(config.Example), 0o600); err != nil {
			return exitError, err
		}
		fmt.Printf("wrote %s\n", path)
	case *showPath:
		fmt.Println(path)
	default:
		cfg, err := config.Load()
		if err != nil {
			return exitError, err
		}
		s := ui.NewStyle(os.Stdout)
		if cfg.Path() == "" {
			fmt.Printf("%s %s\n", s.Dim("no configuration file at"), path)
		} else {
			fmt.Printf("%s %s\n", s.Dim("configuration:"), cfg.Path())
		}
		fmt.Printf("%s %s\n", s.Dim("roots:"), join(cfg.Roots, ", "))
		fmt.Printf("%s %s\n", s.Dim("stale after:"), (time.Duration(cfg.StaleAfterHours) * time.Hour).String())
		fmt.Printf("%s %d restic, Time Machine %v\n", s.Dim("repositories:"),
			len(cfg.Restic), cfg.TimeMachineEnabled())
		st, err := state.Dir()
		if err == nil {
			fmt.Printf("%s %s\n", s.Dim("receipts:"), st)
		}
	}
	return exitOK, nil
}
