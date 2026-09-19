#!/bin/bash
# Cross-compiles restorable and its tests for Linux, copies them to a machine
# given as $1 (user@host), and runs them there as the user and with sudo. Linux
# is a different machine, not a build target: restic is a different version, the
# filesystem is not APFS, and root sees files the user cannot.
set -u
cd "$(dirname "$0")/.."
GO=${GO:-/opt/homebrew/bin/go}
host=${1:-}
[ -n "$host" ] || { echo "usage: scripts/test-linux.sh user@host"; exit 2; }

stage=$(mktemp -d)
trap 'rm -rf "$stage"' EXIT
remote="restorable-test-$$"

echo "→ building for linux/amd64"
GOOS=linux GOARCH=amd64 "$GO" build -o "$stage/restorable" ./cmd/restorable || exit 1
for pkg in $("$GO" list ./... | grep -v '/cmd/'); do
	name=$(echo "${pkg#github.com/morass/restorable/}" | tr '/' '-')
	GOOS=linux GOARCH=amd64 "$GO" test -c -o "$stage/test-$name" "$pkg" 2>/dev/null
done
ls "$stage" | sed 's/^/    /'

echo "→ copying to $host:$remote"
ssh "$host" "rm -rf ~/$remote && mkdir -p ~/$remote" || exit 1
scp -q "$stage"/* "$host:$remote/" || exit 1

echo "→ running as the user"
ssh "$host" "cd ~/$remote && \
	export RESTORABLE_BIN=\$PWD/restorable TMPDIR=\$PWD/tmp && mkdir -p \$PWD/tmp && \
	fail=0; \
	for t in test-*; do printf '%-34s' \"\$t\"; \
		if ./\$t -test.count=1 > \$t.log 2>&1; then echo ok; else echo FAIL; tail -20 \$t.log | sed 's/^/        /'; fail=1; fi; \
	done; \
	echo '--- the binary itself'; ./restorable version; ./restorable backends; \
	exit \$fail"
user=$?

echo "→ running with sudo (root sees a different machine)"
ssh "$host" "cd ~/$remote && sudo -n env RESTORABLE_BIN=\$PWD/restorable TMPDIR=\$PWD/tmp \
	./test-e2e -test.count=1 > sudo-e2e.log 2>&1 && echo 'ok  e2e as root' || { echo 'FAIL e2e as root'; tail -20 sudo-e2e.log | sed 's/^/        /'; }; \
	sudo -n ./restorable doctor; echo \"doctor as root exited \$?\""
root=$?

echo "→ cleaning up"
ssh "$host" "rm -rf ~/$remote"
[ "$user" -eq 0 ] || { echo "✗ tests failed on linux"; exit 1; }
echo "✓ linux tests passed"
