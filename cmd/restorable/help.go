package main

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
)

type topic struct {
	summary string
	body    string
}

var topics = map[string]topic{
	"coverage": {
		summary: "show what on this machine no backup keeps",
		body: `restorable coverage [flags] [path...]

Walks the paths (your home directory by default), asks every backup destination
whether it covers each directory, and lists the largest places nothing keeps.

A directory is a hole when no destination claims it, or when a destination claims
it and excludes it — and then the reason is printed, as far as the system exposes
it. A destination that is configured but not connected keeps nothing right now,
and is reported that way rather than counted as cover.

Examples
  restorable coverage                      walk the configured roots
  restorable coverage ~/Documents          walk one directory
  restorable coverage --min-size 100MB     only the holes worth acting on
  restorable coverage --json               the same answer for a script
  restorable coverage --strict             exit 1 when anything is unprotected

Flags
  --json              print JSON instead of a table
  --depth N           how deep to look inside protected trees (default 8)
  --min-size SIZE     ignore holes smaller than this (default 1MB)
  --files             also ask about individual files, not only directories
  --all               include the folders macOS guards behind a permission prompt
  --cross-filesystems walk onto other mounted volumes as well
  --strict            exit 1 when unprotected data was found
  --quiet             no progress line while walking`,
	},
	"doctor": {
		summary: "say whether each backup destination is readable and fresh",
		body: `restorable doctor [flags]

Lists every destination restorable found, when it last completed, where that date
came from, and what is wrong: never completed, stale, locked behind a passphrase
this session does not have, or simply not there.

Exits 1 when any destination has a problem, so it can run from cron.

Examples
  restorable doctor
  restorable doctor --stale-after 24h
  restorable doctor --json

Flags
  --json             print JSON instead of a table
  --stale-after DUR  when a backup counts as stale (default 48h, or the config)`,
	},
	"drill": {
		summary: "restore a sample of files and compare the bytes",
		body: `restorable drill [flags]

Picks files at random from the newest snapshot, restores them into a temporary
directory, and compares them with what is on the live disk. Nothing is ever
written towards the backup, and the copies are removed unless --keep is given.

A file that was edited after the snapshot is reported as changed, not as a
failure. A file whose bytes differ although it has not been edited, or that the
backup will not hand back at all, is a failure and exits 1. If nothing could be
compared at all — every sampled file gone, edited or unreadable — the drill is
inconclusive, which is also not a pass.

By default the sample is taken from your home directory inside the snapshot,
because a backup holds the whole machine and the files you care about are yours.
The restore always lands in a directory the drill makes for itself, so --target
is a place to put that directory, never a place it writes into directly.

Every drill writes a receipt under the state directory; restorable history lists
them.

Examples
  restorable drill                    drill the freshest destination
  restorable drill --count 40         sample more files
  restorable drill --dest "backup disk"
  restorable drill --path ~/Documents
  restorable drill --keep --target /tmp/drill
  restorable drill --json

Flags
  --dest NAME      which destination to drill (default: the freshest readable one)
  --path DIR       sample from this part of the snapshot (default: your home directory)
  --count N        how many files to sample (default 12)
  --seed N         sample the same files again
  --max-size SIZE  skip files larger than this (default 32MB)
  --target DIR     restore into this directory instead of a temporary one
  --keep           keep the restored copies
  --json           print JSON instead of a table`,
	},
	"history": {
		summary: "list past drills",
		body: `restorable history [flags]

Prints the receipts of past drills, newest first: when, which destination, which
snapshot, how many files were sampled and how it went.

Flags
  --limit N   show at most N receipts (default 20)
  --json      print JSON instead of a table`,
	},
	"backends": {
		summary: "show which backup systems restorable can read here",
		body: `restorable backends [flags]

Lists the backup systems restorable knows, whether their tool is installed, what
they are configured to write to, and what each destination covers.

Flags
  --json   print JSON instead of a table`,
	},
	"config": {
		summary: "show where the configuration lives, and an example",
		body: `restorable config [flags]

restorable works with no configuration at all: it reads Time Machine on a Mac and
the restic repository your shell already points at. A configuration file adds more
repositories and changes the roots it walks.

Flags
  --path      print the path of the configuration file
  --example   print an example configuration
  --write     write the example, if there is no file yet`,
	},
}

func usage(w io.Writer) {
	fmt.Fprint(w, `restorable — is this machine actually backed up?

  restorable coverage      what here is protected by nothing
  restorable doctor        whether each destination is readable and fresh
  restorable drill         restore a sample and compare the bytes
  restorable history       past drills
  restorable backends      which backup systems can be read here
  restorable config        where the configuration lives
  restorable version       print the version

  restorable help COMMAND  the full help for one command

It reads backups; it never writes to them.
`)
}

func help(args []string) int {
	if len(args) == 0 {
		usage(os.Stdout)
		fmt.Println("\nCommands:")
		names := make([]string, 0, len(topics))
		for n := range topics {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			fmt.Printf("  %-10s %s\n", n, topics[n].summary)
		}
		return exitOK
	}
	name := strings.TrimLeft(args[0], "-")
	t, ok := topics[name]
	if !ok {
		fmt.Fprintf(os.Stderr, "restorable: no help for %q\n", args[0])
		return exitUsage
	}
	fmt.Println(t.body)
	return exitOK
}
