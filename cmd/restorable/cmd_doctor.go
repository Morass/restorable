package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/morass/restorable/internal/app"
	"github.com/morass/restorable/internal/backend"
	"github.com/morass/restorable/internal/ui"
)

func cmdDoctor(ctx context.Context, a *app.App, args []string) (int, error) {
	fs := newFlagSet("doctor")
	asJSON := fs.Bool("json", false, "print JSON")
	staleAfter := fs.Duration("stale-after", 0, "when a backup counts as stale")
	if err := fs.Parse(args); err != nil {
		return exitUsage, nil
	}
	if *staleAfter > 0 {
		a.Config.StaleAfterHours = int((*staleAfter).Hours())
		if a.Config.StaleAfterHours < 1 {
			a.Config.StaleAfterHours = 1
		}
	}
	rep, err := a.Doctor(ctx, time.Now())
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
		printDoctor(rep)
	}
	if rep.Problems > 0 {
		return exitProblem, nil
	}
	return exitOK, nil
}

func printDoctor(rep app.Doctor) {
	s := ui.NewStyle(os.Stdout)
	width := ui.TerminalWidth(os.Stdout)
	fmt.Println(s.Bold("Destinations"))

	t := ui.Table{
		Head:  []string{s.Dim("BACKUP"), s.Dim("WHERE"), s.Dim("STATE"), s.Dim("LAST BACKUP"), s.Dim("LAST DRILL")},
		Align: []bool{false, false, false, false, false},
	}
	for _, h := range rep.Health {
		d := h.Dest
		where := d.Label
		if where == "" {
			where = d.ID
		}
		if where == "" {
			where = "—"
		}
		state := s.Green("ok")
		switch {
		case h.NotUsed:
			state = s.Dim("not used")
		case d.State == backend.StateLocked:
			state = s.Yellow("locked")
		case d.State == backend.StateUnreachable:
			state = s.Red("not there")
		case d.State == backend.StateError:
			state = s.Red("error")
		case h.Stale:
			state = s.Yellow("stale")
		case h.Problem != "":
			state = s.Yellow("check")
		}
		last := "—"
		if !d.LastOK.IsZero() {
			last = ui.Age(time.Since(d.LastOK))
		}
		drill := s.Dim("never")
		if h.LastDrill != nil {
			drill = ui.Age(time.Since(*h.LastDrill))
		}
		t.Rows = append(t.Rows, []string{
			string(d.Backend), ui.Path(where, max(24, width/3)), state, last, drill,
		})
	}
	fmt.Print(indent(t.String(), "  "))

	fmt.Println()
	if rep.Problems == 0 {
		fmt.Printf("  %s every destination is readable and fresher than %s\n", s.Green("✓"), ui.Duration(rep.StaleAfter))
		fmt.Println()
		fmt.Println(s.Dim("  Fresh is not the same as restorable: run `restorable drill` to prove the bytes come back."))
		return
	}
	fmt.Println(s.Bold("What is wrong"))
	for _, h := range rep.Health {
		if h.Problem == "" {
			continue
		}
		where := h.Dest.Label
		if where == "" {
			where = h.Dest.ID
		}
		if where == "" {
			where = string(h.Dest.Backend)
		}
		fmt.Printf("  %s %s: %s\n", s.Red("✗"), ui.Path(where, max(20, width/3)), h.Problem)
		if hint := doctorHint(h); hint != "" {
			fmt.Printf("    %s\n", s.Dim(hint))
		}
	}
}

func doctorHint(h app.Health) string {
	switch h.Dest.State {
	case backend.StateLocked:
		return "restorable never stores a passphrase: set RESTIC_PASSWORD_COMMAND, or password_command in the config, to the command you already use."
	case backend.StateUnreachable:
		if h.Dest.Backend == backend.TimeMachine {
			return "connect the backup disk, or `tmutil setdestination` to configure one."
		}
		return "mount the disk the repository lives on, or correct its path in the config."
	}
	if h.Stale {
		return "run the backup, or change --stale-after if this is how often you back up."
	}
	return ""
}
