#!/usr/bin/env bash
# dun launcher — what `make install` puts on your PATH.
#
# A script rather than the binary, because a binary on PATH is only as fresh as
# the last time someone remembered to reinstall it. The in-binary self-update
# (cmd/dun/selfupdate.go) covers the stamped build, but a plain
# `go install ./cmd/dun` leaves srcDir empty and silently never updates again —
# which is exactly how a two-week-old dun ends up running against today's tree.
#
# So: rebuild if anything changed, then exec. exec, not a subshell, so stdin,
# the tty, signals, argv and the exit code all pass straight through — the
# wrapper is not in the process tree once dun is running.
#
# Installed with __SRCDIR__ substituted. Escape hatches:
#   DUN_NO_AUTOBUILD=1   skip the freshness check entirely (fastest launch)
#   DUN_BIN              where the built binary lives
set -uo pipefail

SRCDIR="__SRCDIR__"
BIN="${DUN_BIN:-${XDG_CACHE_HOME:-$HOME/.cache}/dun/dun}"

die() {
	echo "dun: $*" >&2
	exit 127
}

# Source dirs to watch: this repo, plus any LOCAL replace target in go.mod.
# Without the second part, editing ../agentkit would not rebuild dun, and
# "always the latest" would be false in the one case that matters most here —
# dun's engine lives in that replaced module.
watch_dirs() {
	printf '%s\n' "$SRCDIR"
	[ -f "$SRCDIR/go.mod" ] || return 0
	sed -n 's|^[[:space:]]*.*=>[[:space:]]*\([./][^[:space:]]*\).*|\1|p' "$SRCDIR/go.mod" |
		while read -r p; do
			case "$p" in
			/*) [ -d "$p" ] && printf '%s\n' "$p" ;;
			*) [ -d "$SRCDIR/$p" ] && (cd "$SRCDIR/$p" && pwd) ;;
			esac
		done
}

# stale reports whether any watched .go / go.mod / go.sum is newer than the
# binary. Cheap: find bails on the first hit (-print -quit), and hidden
# directories (.git, .poly-lsp-mcp, build caches) are pruned.
stale() {
	[ -x "$BIN" ] || return 0
	local dir hit
	while read -r dir; do
		hit=$(find "$dir" \
			-name '.*' -prune -o \
			\( -name '*.go' -o -name 'go.mod' -o -name 'go.sum' \) \
			-newer "$BIN" -print -quit 2>/dev/null)
		[ -n "$hit" ] && return 0
	done < <(watch_dirs)
	return 1
}

rebuild() {
	local ver tmp
	ver=$(git -C "$SRCDIR" describe --tags --always --dirty 2>/dev/null || echo dev)
	tmp="$BIN.new.$$"
	mkdir -p "$(dirname "$BIN")" || die "cannot create $(dirname "$BIN")"
	if (cd "$SRCDIR" && go build \
		-ldflags "-X main.version=$ver -X main.srcDir=$SRCDIR" \
		-o "$tmp" ./cmd/dun); then
		# Rename, so a concurrent launch either sees the old binary or the new
		# one — never a half-written file.
		mv -f "$tmp" "$BIN"
		return 0
	fi
	rm -f "$tmp"
	return 1
}

[ -d "$SRCDIR" ] || die "source tree $SRCDIR is gone — reinstall with 'make install' from a checkout"

if [ "${DUN_NO_AUTOBUILD:-}" != "1" ] && stale; then
	command -v go >/dev/null || die "go is not on PATH — cannot rebuild"
	echo "dun: source changed — rebuilding…" >&2
	if ! rebuild; then
		# A broken tree should not cost you the tool. Same call the in-binary
		# updater makes.
		if [ -x "$BIN" ]; then
			echo "dun: rebuild failed — running the previous build" >&2
		else
			die "rebuild failed and there is no previous build"
		fi
	fi
fi

[ -x "$BIN" ] || die "no binary at $BIN — run 'make install' from $SRCDIR"
exec "$BIN" "$@"
