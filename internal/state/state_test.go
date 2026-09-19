package state_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/morass/restorable/internal/backend"
	"github.com/morass/restorable/internal/drill"
	"github.com/morass/restorable/internal/state"
)

func receipt(dest string, at time.Time, pass bool) drill.Receipt {
	return drill.Receipt{
		Tool: "restorable", RanAt: at, Backend: backend.Restic, Destination: dest,
		Snapshot: "abc123", Pass: pass,
		Files: []drill.FileResult{{Path: "/x", Status: drill.Match}},
	}
}

func TestReceiptsAreKeptPrivateAndReadBackNewestFirst(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("RESTORABLE_STATE_DIR", dir)

	older := receipt("/repo", time.Now().Add(-2*time.Hour), true)
	newer := receipt("/repo", time.Now().Add(-time.Hour), false)
	p1, err := state.SaveReceipt(older)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := state.SaveReceipt(newer); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(p1)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("receipt mode = %04o, want 0600: it lists the user's paths", perm)
	}
	if !filepath.HasPrefix(p1, dir) {
		t.Errorf("receipt written to %q, outside the state directory", p1)
	}

	got, err := state.Receipts(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("read %d receipts, want 2", len(got))
	}
	if got[0].Pass {
		t.Error("receipts must come back newest first")
	}
	if got[0].Snapshot != "abc123" {
		t.Errorf("snapshot = %q, want it read back", got[0].Snapshot)
	}
}

func TestLimitAndLastDrill(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("RESTORABLE_STATE_DIR", dir)
	for i := 0; i < 5; i++ {
		if _, err := state.SaveReceipt(receipt("/repo", time.Now().Add(-time.Duration(i)*time.Hour), true)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := state.SaveReceipt(receipt("/other", time.Now().Add(-30*time.Minute), true)); err != nil {
		t.Fatal(err)
	}
	got, err := state.Receipts(2)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Errorf("read %d receipts with a limit of 2", len(got))
	}
	if _, ok := state.LastDrill("/repo"); !ok {
		t.Error("a drilled destination must have a last drill")
	}
	if _, ok := state.LastDrill("/never-drilled"); ok {
		t.Error("a destination that was never drilled must not report one")
	}
	if _, ok := state.LastDrill(""); ok {
		t.Error("a destination with no identity has never been drilled")
	}
}

func TestNoStateDirectoryYetIsNotAnError(t *testing.T) {
	t.Setenv("RESTORABLE_STATE_DIR", filepath.Join(t.TempDir(), "not-created"))
	got, err := state.Receipts(0)
	if err != nil {
		t.Fatalf("a machine that has never drilled must read as empty: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("read %d receipts from nothing", len(got))
	}
}
