package app_test

import (
	"context"
	"testing"
	"time"

	"github.com/morass/restorable/internal/app"
	"github.com/morass/restorable/internal/backend"
	"github.com/morass/restorable/internal/backend/fake"
	"github.com/morass/restorable/internal/config"
	"github.com/morass/restorable/internal/drill"
	"github.com/morass/restorable/internal/state"
)

func newApp(t *testing.T, bs ...backend.Backend) *app.App {
	t.Helper()
	t.Setenv("RESTORABLE_STATE_DIR", t.TempDir())
	a := &app.App{Config: config.Config{StaleAfterHours: 48, Roots: []string{t.TempDir()}}, Version: "test"}
	a.Backends = bs
	return a
}

func withLast(b *fake.Backend, when time.Time) *fake.Backend {
	b.Dest.LastOK = when
	b.Dest.LastOKSource = "test"
	return b
}

func TestAFreshDestinationHasNoProblem(t *testing.T) {
	b := withLast(fake.New(backend.Restic, "disk", []string{"/"}, "s1", nil), time.Now().Add(-2*time.Hour))
	rep, err := newApp(t, b).Doctor(context.Background(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Problems != 0 {
		t.Errorf("problems = %d, want none: %+v", rep.Problems, rep.Health)
	}
	if rep.Health[0].LastDrill != nil {
		t.Error("a destination that was never drilled must not claim a drill")
	}
}

func TestAStaleDestinationIsAProblemWithItsAge(t *testing.T) {
	b := withLast(fake.New(backend.Restic, "disk", []string{"/"}, "s1", nil), time.Now().Add(-100*time.Hour))
	rep, err := newApp(t, b).Doctor(context.Background(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Problems != 1 || !rep.Health[0].Stale {
		t.Fatalf("health = %+v, want one stale destination", rep.Health)
	}
	if rep.Health[0].Problem == "" {
		t.Error("a stale destination must say how old it is")
	}
}

func TestADestinationThatNeverCompletedIsAProblem(t *testing.T) {
	b := fake.New(backend.Restic, "disk", []string{"/"}, "s1", nil) // LastOK zero
	rep, err := newApp(t, b).Doctor(context.Background(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Problems != 1 {
		t.Fatalf("problems = %d, want 1", rep.Problems)
	}
	if rep.Health[0].Problem != "it has never completed a backup" {
		t.Errorf("problem = %q", rep.Health[0].Problem)
	}
}

func TestAnUnusedBackupSystemIsOnlyAProblemWhenNothingElseWorks(t *testing.T) {
	tm := fake.New(backend.TimeMachine, "Time Machine", nil, "s1", nil)
	tm.Dest.State = backend.StateUnreachable
	tm.Dest.NotConfigured = true
	tm.Dest.Err = "no Time Machine destination is configured on this Mac"
	res := withLast(fake.New(backend.Restic, "disk", []string{"/"}, "s1", nil), time.Now())

	rep, err := newApp(t, tm, res).Doctor(context.Background(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Problems != 0 {
		t.Errorf("problems = %d, want none while restic is fresh: %+v", rep.Problems, rep.Health)
	}
	var sawNotUsed bool
	for _, h := range rep.Health {
		if h.NotUsed {
			sawNotUsed = true
		}
	}
	if !sawNotUsed {
		t.Error("the unused system must be marked as not used")
	}

	// On a machine where it is the only backup system, it is the whole problem.
	rep, err = newApp(t, tm).Doctor(context.Background(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Problems != 1 {
		t.Errorf("problems = %d, want 1 when nothing else backs up: %+v", rep.Problems, rep.Health)
	}
}

func TestNoBackupSystemAtAllIsTheLoudestProblem(t *testing.T) {
	rep, err := newApp(t).Doctor(context.Background(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Problems != 1 {
		t.Fatalf("problems = %d, want 1", rep.Problems)
	}
	if rep.Health[0].Problem != "no backup system was found on this machine at all" {
		t.Errorf("problem = %q", rep.Health[0].Problem)
	}
}

func TestDrillablePicksTheFreshestReadableDestination(t *testing.T) {
	old := withLast(fake.New(backend.Restic, "old", []string{"/"}, "s-old", nil), time.Now().Add(-48*time.Hour))
	old.Snaps[0].Time = time.Now().Add(-48 * time.Hour)
	fresh := withLast(fake.New(backend.Restic, "fresh", []string{"/"}, "s-fresh", nil), time.Now().Add(-time.Hour))
	fresh.Snaps[0].Time = time.Now().Add(-time.Hour)
	locked := withLast(fake.New(backend.Restic, "locked", []string{"/"}, "s-locked", nil), time.Now())
	locked.Dest.State = backend.StateLocked

	a := newApp(t, old, fresh, locked)
	pair, snap, err := a.Drillable(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if pair.Dest.Label != "fresh" {
		t.Errorf("picked %q, want the freshest readable destination", pair.Dest.Label)
	}
	if snap.ID != "s-fresh" {
		t.Errorf("snapshot = %q", snap.ID)
	}

	if _, _, err := a.Drillable(context.Background(), "locked"); err == nil {
		t.Error("a locked destination cannot be drilled and must say so")
	}
	if _, _, err := a.Drillable(context.Background(), "nothing-called-this"); err == nil {
		t.Error("an unknown destination name must be an error")
	}
}

func TestDrillWritesAReceiptThatHistoryFinds(t *testing.T) {
	root := t.TempDir()
	a := newApp(t)
	b := withLast(fake.New(backend.Restic, "disk", []string{root}, "s1", map[string][]byte{}), time.Now())
	a.Backends = []backend.Backend{b}

	pair, snap, err := a.Drillable(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	rec, path, err := a.Drill(context.Background(), pair, snap, drill.Options{Count: 3, Seed: 5})
	if err != nil {
		t.Fatal(err)
	}
	if path == "" {
		t.Error("a drill must leave a receipt")
	}
	if rec.Version != "test" {
		t.Errorf("version = %q, want the tool's own", rec.Version)
	}
	saved, err := state.Receipts(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(saved) != 1 {
		t.Fatalf("receipts = %d, want 1", len(saved))
	}
	if saved[0].Destination != pair.Dest.ID {
		t.Errorf("receipt destination = %q, want %q", saved[0].Destination, pair.Dest.ID)
	}

	// And the next doctor knows the machine has been drilled.
	rep, err := a.Doctor(context.Background(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Health[0].LastDrill == nil {
		t.Error("doctor must report the drill that just happened")
	}
}
