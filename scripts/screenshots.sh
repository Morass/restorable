#!/bin/bash
# Regenerates the README pictures: a throwaway home with a real restic
# repository, a Mac with no Time Machine destination, and the three commands
# photographed in a real terminal. Nothing touches the real machine.
set -eu
TOOL=restorable
REPO=$(cd "$(dirname "$0")/.." && pwd)
GO=${GO:-/opt/homebrew/bin/go}
OUT="$REPO/docs/images"
W=$(mktemp -d /tmp/shots.XXXXXX)
DEMO_HOME=/tmp/restorabledemo
DISPLAY_HOME=$(printf '%-*s' "${#DEMO_HOME}" '~')
SOCK="$W/tmux.sock"
TMUX_BIN=$(command -v tmux)
T="$TMUX_BIN -S $SOCK"
cleanup() { $T kill-server 2>/dev/null || true; rm -rf "$W" "$DEMO_HOME"; }
trap cleanup EXIT
rm -rf "$DEMO_HOME"
mkdir -p "$OUT" "$W/bin" "$DEMO_HOME"

command -v restic >/dev/null || { echo "screenshots need restic installed"; exit 1; }
(cd "$REPO" && "$GO" build -o "$W/bin/$TOOL" ./cmd/$TOOL && "$GO" build -o "$W/ansi2svg" ./scripts/ansi2svg)
nap() { perl -e "select(undef,undef,undef,$1)"; }

# ---- a believable home: some of it backed up, some of it not ----
mk() { mkdir -p "$(dirname "$1")"; head -c "$2" /dev/urandom > "$1"; }
mk "$DEMO_HOME/Documents/taxes-2026.pdf" 2200000
mk "$DEMO_HOME/Documents/notes.md" 18000
mk "$DEMO_HOME/Documents/contracts/lease.pdf" 900000
mk "$DEMO_HOME/Pictures/2026-summer/IMG_2841.jpg" 4100000
mk "$DEMO_HOME/Pictures/2026-summer/IMG_2842.jpg" 3800000
mk "$DEMO_HOME/code/webshop/main.go" 24000
mk "$DEMO_HOME/code/webshop/go.mod" 400
mk "$DEMO_HOME/Movies/wedding-raw.mov" 78000000
mk "$DEMO_HOME/Library/Caches/com.example.app/blob.bin" 12000000
mk "$DEMO_HOME/VirtualMachines/ubuntu.qcow2" 41000000

export HOME="$DEMO_HOME" TMPDIR="$W/tmp"
mkdir -p "$TMPDIR"
export RESTIC_REPOSITORY="$W/repo" RESTIC_PASSWORD=demo
restic init -q
restic backup -q "$DEMO_HOME/Documents" "$DEMO_HOME/code"
# A second, older snapshot so the history looks like a machine that is used.
restic backup -q "$DEMO_HOME/Documents" "$DEMO_HOME/code" 2>/dev/null || true

cat > "$W/bin/tmutil" <<'STUB'
#!/bin/sh
case "$1" in
destinationinfo) echo '<?xml version="1.0" encoding="UTF-8"?><plist version="1.0"><dict></dict></plist>' ;;
isexcluded) shift; for p in "$@"; do echo "[Included]  $p"; done ;;
*) exit 1 ;;
esac
STUB
chmod 755 "$W/bin/tmutil"

ENV=(env -i HOME="$DEMO_HOME" PATH="$W/bin:/usr/bin:/bin" TERM=xterm-256color LANG=en_US.UTF-8
	BASH_SILENCE_DEPRECATION_WARNING=1 TMPDIR="$TMPDIR"
	RESTIC_REPOSITORY="$RESTIC_REPOSITORY" RESTIC_PASSWORD="$RESTIC_PASSWORD"
	RESTORABLE_TMUTIL="$W/bin/tmutil" RESTORABLE_STATE_DIR="$DEMO_HOME/.state"
	XDG_CONFIG_HOME="$DEMO_HOME/.config" RESTORABLE_FORCE_COLOR=1 RESTORABLE_WIDTH=100)

COLS=100 ROWS=34
"${ENV[@]}" "$TMUX_BIN" -S "$SOCK" -f /dev/null new-session -d -s t -x $COLS -y $ROWS \
	"PS1='\[\e[1;32m\]\$\[\e[0m\] ' bash --noprofile --norc"
$T set -g status off
nap 0.5
type_() { $T send-keys -t t -l "$1"; $T send-keys -t t Enter; }
wait_for() {
	for _ in $(seq 1 100); do
		$T capture-pane -p -t t | grep -qF -- "$1" && { nap 0.4; return 0; }
		nap 0.25
	done
	echo "screenshots: timed out waiting for: $1" >&2
	$T capture-pane -p -t t >&2
	exit 1
}
shot() { # shot NAME "window title"
	$T capture-pane -e -p -t t |
		sed -e "s#$DEMO_HOME#$DISPLAY_HOME#g" -e "s#$W#/tmp/backup#g" |
		"$W/ansi2svg" -title "$2" -cols $COLS >"$OUT/$1.svg"
	# A picture of nothing is worse than no picture: refuse an empty capture.
	if [ "$(wc -c <"$OUT/$1.svg")" -lt 900 ]; then
		echo "screenshots: $1.svg came out empty" >&2
		exit 1
	fi
	echo "  docs/images/$1.svg"
}

type_ "clear; restorable coverage --min-size 1MB"
wait_for "largest holes"
shot coverage "restorable coverage"

type_ "clear; restorable drill --count 6"
wait_for "byte for byte"
shot drill "restorable drill"

type_ "clear; restorable doctor"
wait_for "Destinations"
shot doctor "restorable doctor"

echo "✓ pictures written to docs/images"
