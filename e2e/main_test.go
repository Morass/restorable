// Package e2e drives the real binary in a sandboxed home with real backup tools.
package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

var binary string

func TestMain(m *testing.M) {
	if b := os.Getenv("RESTORABLE_BIN"); b != "" {
		binary = b
		os.Exit(m.Run())
	}
	dir, err := os.MkdirTemp("", "restorable-e2e-*")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	binary = filepath.Join(dir, "restorable")
	build := exec.Command(goTool(), "build", "-o", binary, "./cmd/restorable")
	build.Dir = ".."
	build.Stdout, build.Stderr = os.Stdout, os.Stderr
	if err := build.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "building the binary:", err)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

func goTool() string {
	if g := os.Getenv("GO"); g != "" {
		return g
	}
	if _, err := os.Stat("/opt/homebrew/bin/go"); err == nil {
		return "/opt/homebrew/bin/go"
	}
	return "go"
}

// world is one sandboxed machine: its own HOME, configuration, state directory
// and stub tools.
type world struct {
	t      *testing.T
	Home   string
	Env    map[string]string
	Source string // the "live disk" this machine backs up
}

func newWorld(t *testing.T) *world {
	t.Helper()
	home := t.TempDir()
	w := &world{
		t:    t,
		Home: home,
		Env: map[string]string{
			"HOME":                 home,
			"PATH":                 "/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin",
			"XDG_CONFIG_HOME":      filepath.Join(home, ".config"),
			"RESTORABLE_STATE_DIR": filepath.Join(home, ".state"),
			"RESTORABLE_WIDTH":     "100",
			"NO_COLOR":             "1",
			"TMPDIR":               filepath.Join(home, "tmp"),
			"LANG":                 "C",
		},
		Source: filepath.Join(home, "work"),
	}
	if err := os.MkdirAll(w.Env["TMPDIR"], 0o700); err != nil {
		t.Fatal(err)
	}
	return w
}

// writeFile puts a file on the sandboxed live disk.
func (w *world) writeFile(rel, content string) string {
	w.t.Helper()
	p := filepath.Join(w.Home, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		w.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		w.t.Fatal(err)
	}
	return p
}

type result struct {
	Stdout, Stderr string
	Code           int
}

func (w *world) run(args ...string) result {
	w.t.Helper()
	cmd := exec.Command(binary, args...)
	env := make([]string, 0, len(w.Env))
	for k, v := range w.Env {
		env = append(env, k+"="+v)
	}
	cmd.Env = env
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	res := result{Stdout: out.String(), Stderr: errb.String()}
	if ee, ok := err.(*exec.ExitError); ok {
		res.Code = ee.ExitCode()
	} else if err != nil {
		w.t.Fatalf("running %v: %v", args, err)
	}
	return res
}

// mustRun fails the test when the command did not exit with the wanted code.
func (w *world) mustRun(code int, args ...string) result {
	w.t.Helper()
	res := w.run(args...)
	if res.Code != code {
		w.t.Fatalf("%v exited %d, want %d\nstdout:\n%s\nstderr:\n%s", args, res.Code, code, res.Stdout, res.Stderr)
	}
	return res
}

// restic sets up a real restic repository and backs up the source tree.
func (w *world) restic(paths ...string) string {
	w.t.Helper()
	if _, err := exec.LookPath("restic"); err != nil {
		w.t.Skip("restic is not installed")
	}
	repo := filepath.Join(w.Home, "repo")
	w.Env["RESTIC_REPOSITORY"] = repo
	w.Env["RESTIC_PASSWORD"] = "e2e-pass"
	w.resticCmd("init")
	w.resticCmd(append([]string{"backup", "--quiet"}, paths...)...)
	return repo
}

func (w *world) resticCmd(args ...string) string {
	w.t.Helper()
	cmd := exec.Command("restic", args...)
	cmd.Env = []string{
		"HOME=" + w.Home,
		"PATH=/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin",
		"RESTIC_REPOSITORY=" + w.Env["RESTIC_REPOSITORY"],
		"RESTIC_PASSWORD=" + w.Env["RESTIC_PASSWORD"],
		"TMPDIR=" + w.Env["TMPDIR"],
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		w.t.Fatalf("restic %v: %v\n%s", args, err, out)
	}
	return string(out)
}

// stubTool installs a fake external tool for this world.
func (w *world) stubTool(envVar, name, script string) string {
	w.t.Helper()
	dir := filepath.Join(w.Home, "stubs")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		w.t.Fatal(err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		w.t.Fatal(err)
	}
	w.Env[envVar] = p
	return p
}

// noTimeMachine installs a tmutil that reports a Mac with no destination, so a
// test is not at the mercy of the machine it runs on.
func (w *world) noTimeMachine() {
	w.stubTool("RESTORABLE_TMUTIL", "tmutil", `case "$1" in
destinationinfo) echo '<?xml version="1.0" encoding="UTF-8"?><plist version="1.0"><dict></dict></plist>';;
*) exit 1;;
esac`)
}

func decode[T any](t *testing.T, s string) T {
	t.Helper()
	var v T
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, s)
	}
	return v
}

func contains(t *testing.T, haystack, needle string) {
	t.Helper()
	if !strings.Contains(haystack, needle) {
		t.Errorf("output does not mention %q:\n%s", needle, haystack)
	}
}

func absent(t *testing.T, haystack, needle string) {
	t.Helper()
	if strings.Contains(haystack, needle) {
		t.Errorf("output should not mention %q:\n%s", needle, haystack)
	}
}
