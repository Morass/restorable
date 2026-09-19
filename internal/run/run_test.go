package run_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/morass/restorable/internal/run"
)

// stub writes an executable script and points a tool's environment variable at it.
func stub(t *testing.T, tool run.Tool, script string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), tool.Name+"-stub")
	body := "#!/bin/sh\n" + script
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(tool.EnvVar, path)
}

func TestStubReplacesTheRealTool(t *testing.T) {
	stub(t, run.Tmutil, `echo "[Included]  $2"`)
	res, err := run.Runner{}.Run(context.Background(), run.Tmutil, "isexcluded", "/tmp")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(res.Stdout)); got != "[Included]  /tmp" {
		t.Errorf("stdout = %q", got)
	}
	if res.ExitCode != 0 {
		t.Errorf("exit = %d", res.ExitCode)
	}
}

func TestANonZeroExitIsAnAnswerNotAnError(t *testing.T) {
	stub(t, run.Restic, `echo "no key found" >&2; exit 1`)
	res, err := run.Runner{}.Run(context.Background(), run.Restic, "snapshots")
	if err != nil {
		t.Fatalf("a tool that exits non-zero must not be an error: %v", err)
	}
	if res.ExitCode != 1 {
		t.Errorf("exit = %d, want 1", res.ExitCode)
	}
	if !strings.Contains(string(res.Stderr), "no key found") {
		t.Errorf("stderr = %q", res.Stderr)
	}
}

func TestATimeoutIsAnError(t *testing.T) {
	stub(t, run.Restic, `sleep 5`)
	start := time.Now()
	_, err := run.Runner{Timeout: 200 * time.Millisecond}.Run(context.Background(), run.Restic, "snapshots")
	if err == nil {
		t.Fatal("a tool that hangs must time out")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("error = %v, want it to say it timed out", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("the timeout took %s to take effect", d)
	}
}

func TestOnlyTheToolsOwnVariablesAreForwarded(t *testing.T) {
	stub(t, run.Restic, `env`)
	t.Setenv("RESTIC_PASSWORD_COMMAND", "echo hunter2")
	t.Setenv("MY_PRIVATE_TOKEN", "sk-do-not-forward")
	res, err := run.Runner{Env: []string{"RESTIC_REPOSITORY=/repo"}}.Run(context.Background(), run.Restic, "snapshots")
	if err != nil {
		t.Fatal(err)
	}
	env := string(res.Stdout)
	if !strings.Contains(env, "RESTIC_PASSWORD_COMMAND=echo hunter2") {
		t.Error("the tool's own variables must be forwarded")
	}
	if !strings.Contains(env, "RESTIC_REPOSITORY=/repo") {
		t.Error("the runner's own variables must be forwarded")
	}
	if strings.Contains(env, "MY_PRIVATE_TOKEN") {
		t.Error("the session's other variables must not reach a backup tool")
	}
	if !strings.Contains(env, "LC_ALL=C") {
		t.Error("output parsing depends on a fixed locale")
	}
}

func TestMachineAccessCanBeSwitchedOff(t *testing.T) {
	t.Setenv("RESTORABLE_NO_MACHINE", "1")
	if run.Tmutil.Available() {
		t.Error("with machine access off and no stub, a tool must not be available")
	}
	_, err := run.Runner{}.Run(context.Background(), run.Tmutil, "destinationinfo")
	if err == nil || !strings.Contains(err.Error(), "switched off") {
		t.Errorf("error = %v, want it to say machine access is off", err)
	}
	// A stub still works, so tests and screenshots can run with it on.
	stub(t, run.Tmutil, `echo stubbed`)
	res, err := run.Runner{}.Run(context.Background(), run.Tmutil, "destinationinfo")
	if err != nil {
		t.Fatalf("a stub must still run with machine access off: %v", err)
	}
	if !strings.Contains(string(res.Stdout), "stubbed") {
		t.Errorf("stdout = %q", res.Stdout)
	}
}

func TestAMissingToolSaysSo(t *testing.T) {
	t.Setenv("RESTORABLE_PATH", t.TempDir())
	t.Setenv("PATH", t.TempDir())
	if run.Borg.Available() {
		t.Skip("borg found on an empty PATH, which cannot happen")
	}
	_, err := run.Runner{}.Run(context.Background(), run.Borg, "list")
	if err == nil || !strings.Contains(err.Error(), "not installed") {
		t.Errorf("error = %v, want 'not installed'", err)
	}
}

func TestStreamReadsLineByLineAndReportsFailure(t *testing.T) {
	stub(t, run.Restic, `printf 'a\n\nb\nc\n'`)
	var lines []string
	err := run.Runner{}.Stream(context.Background(), run.Restic, []string{"ls"}, func(l []byte) error {
		lines = append(lines, string(l))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(lines, ",") != "a,b,c" {
		t.Errorf("lines = %v, want a,b,c with the empty line skipped", lines)
	}

	stub(t, run.Restic, `echo a; echo "broken repository" >&2; exit 2`)
	err = run.Runner{}.Stream(context.Background(), run.Restic, []string{"ls"}, func([]byte) error { return nil })
	if err == nil {
		t.Fatal("a tool that fails mid-stream must be reported")
	}
	if !strings.Contains(err.Error(), "broken repository") {
		t.Errorf("error = %v, want the tool's own message", err)
	}
}

func TestStreamStopsWhenTheCallbackStops(t *testing.T) {
	stub(t, run.Restic, `i=0; while [ $i -lt 100000 ]; do echo line$i; i=$((i+1)); done`)
	seen := 0
	stopErr := errAfterTen
	err := run.Runner{}.Stream(context.Background(), run.Restic, []string{"ls"}, func([]byte) error {
		seen++
		if seen == 10 {
			return stopErr
		}
		return nil
	})
	if err != stopErr {
		t.Fatalf("error = %v, want the callback's own error", err)
	}
	if seen != 10 {
		t.Errorf("read %d lines after the callback stopped", seen)
	}
}

var errAfterTen = errStop("enough")

type errStop string

func (e errStop) Error() string { return string(e) }
