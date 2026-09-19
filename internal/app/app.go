// Package app holds the operations the command line drives: find the backups,
// judge them, walk for holes, and drill.
package app

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/morass/restorable/internal/backend"
	"github.com/morass/restorable/internal/backend/resticbk"
	"github.com/morass/restorable/internal/backend/tmbk"
	"github.com/morass/restorable/internal/config"
	"github.com/morass/restorable/internal/coverage"
	"github.com/morass/restorable/internal/drill"
	"github.com/morass/restorable/internal/run"
	"github.com/morass/restorable/internal/state"
)

// App is one run of the tool.
type App struct {
	Config   config.Config
	Runner   run.Runner
	Version  string
	Backends []backend.Backend
	// StaleOverride replaces the configured staleness threshold when the command
	// line asked for a different one.
	StaleOverride time.Duration
}

// New builds the app from a configuration, including only the backends this
// machine can actually read.
func New(cfg config.Config, version string) *App {
	a := &App{Config: cfg, Version: version, Runner: run.Runner{Timeout: run.TimeoutFromEnv()}}
	if cfg.TimeMachineEnabled() {
		tm := &tmbk.Backend{Runner: a.Runner}
		if tm.Installed() {
			a.Backends = append(a.Backends, tm)
		}
	}
	if len(cfg.Restic) > 0 {
		var repos []resticbk.Repo
		for _, r := range cfg.Restic {
			repos = append(repos, resticbk.Repo{Name: r.Name, Repo: r.Repo, PasswordCommand: r.PasswordCommand})
		}
		rb := &resticbk.Backend{Repos: repos, Runner: a.Runner}
		if rb.Installed() {
			a.Backends = append(a.Backends, rb)
		}
	}
	return a
}

// Pair is a destination together with the backend that can read it.
type Pair struct {
	Dest    backend.Destination
	Backend backend.Backend
}

// Destinations reads every destination of every backend.
func (a *App) Destinations(ctx context.Context) ([]Pair, error) {
	var out []Pair
	for _, b := range a.Backends {
		dests, err := b.Destinations(ctx)
		if err != nil {
			out = append(out, Pair{Backend: b, Dest: backend.Destination{
				Backend: b.Kind(), State: backend.StateError, Err: err.Error(),
			}})
			continue
		}
		for _, d := range dests {
			out = append(out, Pair{Dest: d, Backend: b})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Dest.Backend != out[j].Dest.Backend {
			return out[i].Dest.Backend < out[j].Dest.Backend
		}
		return out[i].Dest.Label < out[j].Dest.Label
	})
	return out, nil
}

// Health is what doctor says about one destination.
type Health struct {
	Dest    backend.Destination `json:"destination"`
	Age     time.Duration       `json:"age"`
	Stale   bool                `json:"stale"`
	Problem string              `json:"problem,omitempty"`
	// NotUsed marks a backup system that is not set up on a machine that is
	// backed up by something else.
	NotUsed   bool       `json:"not_used,omitempty"`
	LastDrill *time.Time `json:"last_drill,omitempty"`
}

// DoctorReport is the freshness answer.
type Doctor struct {
	GeneratedAt time.Time     `json:"generated_at"`
	StaleAfter  time.Duration `json:"stale_after"`
	Health      []Health      `json:"destinations"`
	Problems    int           `json:"problems"`
	// NothingReadable is true when not one destination could be read, which is
	// the answer that matters more than any single row.
	NothingReadable bool `json:"nothing_readable"`
}

// StaleAfter is how old a backup may be before doctor complains: the override
// the command line was given, or the configured number of hours.
func (a *App) StaleAfter() time.Duration {
	if a.StaleOverride > 0 {
		return a.StaleOverride
	}
	return time.Duration(a.Config.StaleAfterHours) * time.Hour
}

// Doctor judges every destination: readable, fresh, and drilled at some point.
func (a *App) Doctor(ctx context.Context, now time.Time) (Doctor, error) {
	pairs, err := a.Destinations(ctx)
	if err != nil {
		return Doctor{}, err
	}
	stale := a.StaleAfter()
	rep := Doctor{GeneratedAt: now, StaleAfter: stale}
	if len(pairs) == 0 {
		rep.Problems++
		rep.NothingReadable = true
		rep.Health = append(rep.Health, Health{
			Dest:    backend.Destination{Backend: "none", State: backend.StateUnreachable, NotConfigured: true},
			Problem: NoBackupSystem,
		})
		return rep, nil
	}
	readable := 0
	for _, p := range pairs {
		if p.Dest.State == backend.StateOK {
			readable++
		}
	}
	rep.NothingReadable = readable == 0
	for _, p := range pairs {
		h := Health{Dest: p.Dest}
		if t, ok := state.LastDrill(p.Dest.ID); ok {
			tt := t
			h.LastDrill = &tt
		}
		switch p.Dest.State {
		case backend.StateOK:
			if p.Dest.LastOK.IsZero() {
				h.Problem = "it has never completed a backup"
			} else {
				h.Age = now.Sub(p.Dest.LastOK)
				if h.Age > stale {
					h.Stale = true
					h.Problem = fmt.Sprintf("the last backup is %s old", Round(h.Age))
				}
			}
		case backend.StateLocked:
			h.Problem = p.Dest.Err
		case backend.StateUnreachable:
			h.Problem = p.Dest.Err
		default:
			h.Problem = p.Dest.Err
		}
		// A backup system that was never set up is only a problem when nothing
		// else on this machine is backing anything up.
		if p.Dest.NotConfigured && readable > 0 {
			h.Problem, h.NotUsed = "", true
		}
		if h.Problem != "" {
			rep.Problems++
		}
		rep.Health = append(rep.Health, h)
	}
	return rep, nil
}

// Coverage walks for holes, using every destination that could be read.
func (a *App) Coverage(ctx context.Context, opts coverage.Options) (coverage.Report, error) {
	pairs, err := a.Destinations(ctx)
	if err != nil {
		return coverage.Report{}, err
	}
	var prot []*coverage.Protector
	for _, p := range pairs {
		prot = append(prot, &coverage.Protector{Dest: p.Dest, Backend: p.Backend})
	}
	if len(opts.Roots) == 0 {
		opts.Roots = a.Config.Roots
	}
	return coverage.Run(ctx, opts, prot)
}

// Drillable picks the destination to drill: the named one, or the freshest that
// can actually be read.
func (a *App) Drillable(ctx context.Context, name string) (Pair, backend.Snapshot, error) {
	pairs, err := a.Destinations(ctx)
	if err != nil {
		return Pair{}, backend.Snapshot{}, err
	}
	var candidates []Pair
	for _, p := range pairs {
		if name != "" && p.Dest.Label != name && p.Dest.ID != name {
			continue
		}
		if p.Dest.State != backend.StateOK {
			continue
		}
		candidates = append(candidates, p)
	}
	if len(candidates) == 0 {
		if name != "" {
			return Pair{}, backend.Snapshot{}, fmt.Errorf("no readable destination called %q", name)
		}
		return Pair{}, backend.Snapshot{}, fmt.Errorf("no destination could be read, so there is nothing to drill")
	}
	sort.SliceStable(candidates, func(i, j int) bool { return candidates[i].Dest.LastOK.After(candidates[j].Dest.LastOK) })
	p := candidates[0]
	snaps, err := p.Backend.Snapshots(ctx, p.Dest)
	if err != nil {
		return p, backend.Snapshot{}, err
	}
	snap, ok := backend.Newest(snaps)
	if !ok {
		return p, backend.Snapshot{}, fmt.Errorf("%s has no snapshot to drill", p.Dest.Label)
	}
	return p, snap, nil
}

// Drill runs one drill and records the receipt.
func (a *App) Drill(ctx context.Context, p Pair, snap backend.Snapshot, opts drill.Options) (drill.Receipt, string, error) {
	opts.Version = a.Version
	rec, err := drill.Run(ctx, p.Backend, p.Dest, snap, opts)
	if err != nil {
		return rec, "", err
	}
	path, saveErr := state.SaveReceipt(rec)
	return rec, path, saveErr
}

// NoBackupSystem is the problem reported on a machine where nothing at all could
// be read as a backup.
const NoBackupSystem = "no backup system was found on this machine at all"

// Round shortens a duration for reading.
func Round(d time.Duration) time.Duration {
	switch {
	case d > 48*time.Hour:
		return d.Round(time.Hour)
	case d > time.Hour:
		return d.Round(time.Minute)
	}
	return d.Round(time.Second)
}
