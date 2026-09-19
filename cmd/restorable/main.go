// Command restorable answers three questions about the backups of this machine:
// what here is protected by nothing, when each destination last finished, and
// whether a restore actually hands the bytes back.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/morass/restorable/internal/app"
	"github.com/morass/restorable/internal/config"
)

// Version is the build's version, set at link time for a release.
var Version = "dev"

const (
	exitOK      = 0
	exitProblem = 1 // something is wrong with the backups
	exitUsage   = 2
	exitError   = 3 // the tool itself could not do its job
)

func main() {
	os.Exit(runMain(os.Args[1:]))
}

func runMain(args []string) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if len(args) == 0 {
		usage(os.Stdout)
		return exitUsage
	}
	cmd, rest := args[0], args[1:]

	switch cmd {
	case "-h", "--help", "help":
		return help(rest)
	case "-V", "--version", "version":
		fmt.Println("restorable " + Version)
		return exitOK
	}

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "restorable: "+err.Error())
		return exitError
	}
	a := app.New(cfg, Version)

	var runErr error
	code := exitOK
	switch cmd {
	case "coverage":
		code, runErr = cmdCoverage(ctx, a, rest)
	case "doctor":
		code, runErr = cmdDoctor(ctx, a, rest)
	case "drill":
		code, runErr = cmdDrill(ctx, a, rest)
	case "history":
		code, runErr = cmdHistory(rest)
	case "backends":
		code, runErr = cmdBackends(ctx, a, rest)
	case "config":
		code, runErr = cmdConfig(rest)
	default:
		fmt.Fprintf(os.Stderr, "restorable: no command called %q\n\n", cmd)
		usage(os.Stderr)
		return exitUsage
	}
	if runErr != nil {
		if errors.Is(runErr, context.Canceled) {
			fmt.Fprintln(os.Stderr, "restorable: stopped")
			return exitError
		}
		fmt.Fprintln(os.Stderr, "restorable: "+runErr.Error())
		if code == exitOK {
			code = exitError
		}
	}
	return code
}
