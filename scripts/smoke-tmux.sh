#!/bin/bash
# Drives restorable in a real terminal, because a real terminal is the only place
# the progress line, the column widths and the colours are what a user sees.
# Needs tmux and restic. Leaves nothing behind.
set -u
cd "$(dirname "$0")/.."

GO=${GO:-/opt/homebrew/bin/go}
command -v tmux >/dev/null || { echo "skip: tmux is not installed"; exit 0; }
command -v restic >/dev/null || { echo "skip: restic is not installed"; exit 0; }

work=$(mktemp -d)
session="restorable-smoke-$$"
cleanup() {
	tmux kill-session -t "$session" 2>/dev/null
	rm -rf "$work"
}
trap cleanup EXIT

# SMOKE_BIN lets the negative control point this at a binary that does nothing,
# which proves the checks below can actually fail.
bin=${SMOKE_BIN:-"$work/restorable"}
if [ -z "${SMOKE_BIN:-}" ]; then
	"$GO" build -o "$bin" ./cmd/restorable || { echo "✗ build failed"; exit 1; }
fi

home="$work/home"
mkdir -p "$home/data/work" "$home/data/loose" "$home/.state" "$home/tmp"
printf 'kept\n' > "$home/data/work/a.txt"
head -c 200000 /dev/urandom > "$home/data/work/big.bin"
head -c 300000 /dev/urandom > "$home/data/loose/nobody-keeps-this.bin"

export RESTIC_REPOSITORY="$work/repo" RESTIC_PASSWORD=smoke HOME="$home" TMPDIR="$home/tmp"
restic init -q || { echo "✗ restic init failed"; exit 1; }
restic backup -q "$home/data/work" || { echo "✗ restic backup failed"; exit 1; }

# A Mac with no Time Machine destination, so the smoke test says the same thing
# on every machine.
stub="$work/tmutil"
cat > "$stub" <<'STUB'
#!/bin/sh
case "$1" in
destinationinfo) echo '<?xml version="1.0" encoding="UTF-8"?><plist version="1.0"><dict></dict></plist>' ;;
*) exit 1 ;;
esac
STUB
chmod 755 "$stub"

fail=0
check() { # check NAME PATTERN
	if grep -qE "$2" "$work/screen"; then
		echo "✓ $1"
	else
		echo "✗ $1 — no /$2/ on the screen:"
		sed 's/^/    /' "$work/screen"
		fail=1
	fi
}

run_in_tmux() { # run_in_tmux COMMAND...
	tmux kill-session -t "$session" 2>/dev/null
	tmux new-session -d -s "$session" -x 100 -y 40 \
		-e HOME="$home" -e TMPDIR="$home/tmp" \
		-e RESTIC_REPOSITORY="$RESTIC_REPOSITORY" -e RESTIC_PASSWORD="$RESTIC_PASSWORD" \
		-e RESTORABLE_STATE_DIR="$home/.state" -e XDG_CONFIG_HOME="$home/.config" \
		-e RESTORABLE_TMUTIL="$stub" \
		"$@"
	for _ in $(seq 1 100); do
		tmux has-session -t "$session" 2>/dev/null || break
		if tmux capture-pane -p -t "$session" 2>/dev/null | grep -q 'SMOKE-DONE'; then break; fi
		sleep 0.2
	done
	tmux capture-pane -p -t "$session" > "$work/screen" 2>/dev/null
	tmux kill-session -t "$session" 2>/dev/null
}

# Coverage in a real terminal: the table must be aligned and the hole named.
run_in_tmux sh -c "$bin coverage --min-size 1KB $home/data; echo SMOKE-DONE; sleep 30"
check "coverage names the hole"            'nobody-keeps-this|loose'
check "coverage says how much is unkept"   'protected by nothing'
check "coverage shows the destinations"    'restic.*readable'
check "the progress line was erased"       'SMOKE-DONE'
if grep -q 'looking at' "$work/screen"; then
	echo "✗ the progress line was left on the screen"
	fail=1
else
	echo "✓ the progress line did not stay behind"
fi

# A drill in a real terminal, then the receipt in history.
run_in_tmux sh -c "$bin drill --count 4 --seed 7; echo SMOKE-DONE; sleep 30"
check "drill reports a pass"       'came back byte for byte'
check "drill writes a receipt"     'receipt:'

run_in_tmux sh -c "$bin history; echo SMOKE-DONE; sleep 30"
check "history lists the drill"    'pass'

# Colour: a terminal gets escape codes, a pipe does not.
run_in_tmux sh -c "$bin doctor | cat -v | head -30; echo SMOKE-DONE; sleep 30"
check "doctor runs in a pipe"      'Destinations'
if grep -q '\^\[\[' "$work/screen"; then
	echo "✗ colour codes were written into a pipe"
	fail=1
else
	echo "✓ no colour codes when the output is a pipe"
fi

if [ "$fail" -eq 0 ]; then
	echo "✓ terminal smoke test passed"
else
	echo "✗ terminal smoke test failed"
fi
exit "$fail"
