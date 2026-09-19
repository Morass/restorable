// Package run executes the external tools restorable reads (tmutil, restic, borg)
// with a fixed environment, a deadline, and a test switch for every one of them.
package run

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Tool is an external program restorable reads. Each one can be replaced for
// tests and screenshots through its environment variable, and all machine
// access can be switched off at once with RESTORABLE_NO_MACHINE=1.
type Tool struct {
	Name    string   // "tmutil"
	EnvVar  string   // "RESTORABLE_TMUTIL"
	PassEnv []string // extra environment variables forwarded to this tool
}

var (
	Tmutil = Tool{Name: "tmutil", EnvVar: "RESTORABLE_TMUTIL"}
	Plutil = Tool{Name: "plutil", EnvVar: "RESTORABLE_PLUTIL"}
	Restic = Tool{Name: "restic", EnvVar: "RESTORABLE_RESTIC", PassEnv: []string{
		"RESTIC_REPOSITORY", "RESTIC_REPOSITORY_FILE", "RESTIC_PASSWORD",
		"RESTIC_PASSWORD_FILE", "RESTIC_PASSWORD_COMMAND", "RESTIC_CACHE_DIR",
		"RESTIC_COMPRESSION", "RESTIC_PACK_SIZE", "RESTIC_PROGRESS_FPS",
		"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_DEFAULT_REGION",
		"B2_ACCOUNT_ID", "B2_ACCOUNT_KEY", "GOOGLE_PROJECT_ID",
		"GOOGLE_APPLICATION_CREDENTIALS", "AZURE_ACCOUNT_NAME", "AZURE_ACCOUNT_KEY",
	}}
	Borg = Tool{Name: "borg", EnvVar: "RESTORABLE_BORG", PassEnv: []string{
		"BORG_REPO", "BORG_PASSPHRASE", "BORG_PASSCOMMAND", "BORG_PASSPHRASE_FD",
		"BORG_RSH", "BORG_CACHE_DIR", "BORG_CONFIG_DIR", "BORG_BASE_DIR",
	}}
)

// ErrNoMachine is returned when RESTORABLE_NO_MACHINE=1 forbids reading real
// machine state and no stub was provided for the tool.
var ErrNoMachine = errors.New("machine access is switched off (RESTORABLE_NO_MACHINE=1)")

// ErrMissing is returned when the tool is not installed.
var ErrMissing = errors.New("not installed")

// NoMachine reports whether real machine state must not be touched.
func NoMachine() bool { return os.Getenv("RESTORABLE_NO_MACHINE") == "1" }

// Result is what a finished command produced.
type Result struct {
	Stdout   []byte
	Stderr   []byte
	ExitCode int
	Duration time.Duration
}

// Path resolves the program to run for a tool: its stub if one is configured,
// otherwise the installed binary.
func (t Tool) Path() (string, error) {
	if stub := os.Getenv(t.EnvVar); stub != "" {
		if _, err := os.Stat(stub); err != nil {
			return "", fmt.Errorf("%s from %s: %w", t.Name, t.EnvVar, err)
		}
		return stub, nil
	}
	if NoMachine() {
		return "", fmt.Errorf("%s: %w", t.Name, ErrNoMachine)
	}
	// The tool is looked for on the same path it will run with, not on the
	// session's PATH: a backup tool installed in /opt/homebrew/bin must be found
	// even from a shell whose PATH does not mention it.
	if p, ok := lookPath(t.Name); ok {
		return p, nil
	}
	return "", fmt.Errorf("%s: %w", t.Name, ErrMissing)
}

// Available reports whether the tool can be run at all.
func (t Tool) Available() bool { _, err := t.Path(); return err == nil }

// Runner runs tools. The zero value is usable; Timeout defaults to 30s.
type Runner struct {
	Timeout time.Duration
	// Env holds extra variables (name=value) added to every command, used by
	// backends that must point a tool at one repository.
	Env []string
}

const defaultTimeout = 30 * time.Second

// TimeoutFromEnv reads RESTORABLE_TIMEOUT, so a slow network destination can be
// given longer and a test can ask for a short one. An unreadable value is ignored.
func TimeoutFromEnv() time.Duration {
	v := os.Getenv("RESTORABLE_TIMEOUT")
	if v == "" {
		return 0
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return 0
	}
	return d
}

// Run executes the tool and returns its output. A non-zero exit is not an
// error: the caller decides, because several of these tools use exit codes as
// answers. A deadline or a missing tool is an error.
func (r Runner) Run(ctx context.Context, t Tool, args ...string) (Result, error) {
	prog, err := t.Path()
	if err != nil {
		return Result{}, err
	}
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, prog, args...)
	groupKill(cmd)
	cmd.Env = t.environ(r.Env)
	cmd.Stdin = nil
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	start := time.Now()
	runErr := cmd.Run()
	res := Result{Stdout: out.Bytes(), Stderr: errb.Bytes(), Duration: time.Since(start)}

	if ctx.Err() != nil {
		return res, fmt.Errorf("%s %s: timed out after %s", t.Name, strings.Join(args, " "), timeout)
	}
	var ee *exec.ExitError
	switch {
	case runErr == nil:
		res.ExitCode = 0
	case errors.As(runErr, &ee):
		res.ExitCode = ee.ExitCode()
	default:
		return res, fmt.Errorf("%s %s: %w", t.Name, strings.Join(args, " "), runErr)
	}
	return res, nil
}

// environ builds the environment for a tool: a fixed base, the variables the
// tool itself understands if the user set them, and the runner's own additions.
// Nothing else is forwarded, so a backup tool never inherits the whole session.
func (t Tool) environ(extra []string) []string {
	env := []string{
		"PATH=" + pathEnv(),
		"HOME=" + os.Getenv("HOME"),
		"LANG=C",
		"LC_ALL=C",
		"TZ=" + os.Getenv("TZ"),
	}
	if tmp := os.Getenv("TMPDIR"); tmp != "" {
		env = append(env, "TMPDIR="+tmp)
	}
	for _, k := range t.PassEnv {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	return append(env, extra...)
}

// lookPath finds an executable on the path restorable runs tools with.
func lookPath(name string) (string, bool) {
	if strings.ContainsRune(name, os.PathSeparator) {
		if err := executable(name); err == nil {
			return name, true
		}
		return "", false
	}
	for _, dir := range filepath.SplitList(pathEnv()) {
		if dir == "" {
			continue
		}
		candidate := filepath.Join(dir, name)
		if err := executable(candidate); err == nil {
			return candidate, true
		}
	}
	return "", false
}

func executable(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.IsDir() || info.Mode().Perm()&0o111 == 0 {
		return os.ErrPermission
	}
	return nil
}

func pathEnv() string {
	if p := os.Getenv("RESTORABLE_PATH"); p != "" {
		return p
	}
	// Homebrew first: a stub tool in the session PATH must still be reachable,
	// and /usr/bin-first ordering has bitten us before with wrapper binaries.
	base := "/opt/homebrew/bin:/opt/homebrew/sbin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"
	if p := os.Getenv("PATH"); p != "" {
		return p + ":" + base
	}
	return base
}

// Stream runs a tool and calls fn for each non-empty line of its output as it
// arrives, so a listing of a million files never has to be held in memory.
func (r Runner) Stream(ctx context.Context, t Tool, args []string, fn func(line []byte) error) error {
	prog, err := t.Path()
	if err != nil {
		return err
	}
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Minute // a full listing is allowed to take a while
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, prog, args...)
	groupKill(cmd)
	cmd.Env = t.environ(r.Env)
	var errb bytes.Buffer
	cmd.Stderr = &errb
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024) // a JSON node line can be long
	var fnErr error
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		if err := fn(line); err != nil {
			fnErr = err
			break
		}
	}
	scanErr := sc.Err()
	if fnErr != nil {
		_ = cmd.Process.Kill()
	}
	waitErr := cmd.Wait()
	switch {
	case fnErr != nil:
		return fnErr
	case ctx.Err() != nil:
		return fmt.Errorf("%s %s: timed out after %s", t.Name, strings.Join(args, " "), timeout)
	case scanErr != nil:
		return fmt.Errorf("%s %s: %w", t.Name, strings.Join(args, " "), scanErr)
	case waitErr != nil:
		return fmt.Errorf("%s %s: %w: %s", t.Name, strings.Join(args, " "), waitErr, firstLine(errb.Bytes()))
	}
	return nil
}

func firstLine(b []byte) string {
	s := strings.TrimSpace(string(b))
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}
