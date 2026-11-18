#!/usr/bin/env bash
# make test-changed: test the packages a change touches, not the whole module.
#
# A package is picked when a file changed in its directory (or below it, in a
# directory with no Go package of its own: testdata/, embedded files), or when
# it imports such a package, directly or not, in its code or its tests. A
# change to go.mod or go.sum picks every package. "Changed" is everything that
# differs from the merge base with $BASE (default main): commits, staged and
# unstaged edits, and untracked files.
#
# The flags are make test's: none. Extra arguments go to go test
# (tools/test-changed.sh -run TestFoo -v); LIST=1 prints the packages and
# runs nothing. The full make test still runs once, before a merge.
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"
BASE="${BASE:-main}"
base=$(git merge-base "$BASE" HEAD)

changed=$({
	git diff --name-only "$base"
	git ls-files --others --exclude-standard
} | sort -u)

if [ -z "$changed" ]; then
	echo "test-changed: nothing differs from $BASE"
	exit 0
fi

mod=$(go list -m)
# Every package dir of the main module, relative to the root ("." for the root).
mapfile -t dirs < <(go list -f '{{.Dir}}' ./... | sed "s|^$PWD/\{0,1\}||; s|^$|.|")
declare -A isdir=()
for d in "${dirs[@]}"; do isdir[$d]=1; done

all=0
other=() # the other modules in this tree (tools/ttyrec, tools/ttydrive)
declare -A hit=()
while IFS= read -r f; do
	case "$f" in
	go.mod | go.sum) all=1 ;;
	tools/ttyrec/* | tools/ttydrive/*)
		m=${f#tools/}; m=tools/${m%%/*}; other+=("$m") ;;
	*)
		d=$(dirname "$f")
		while [ "$d" != "." ] && [ -z "${isdir[$d]:-}" ]; do d=$(dirname "$d"); done
		[ -n "${isdir[$d]:-}" ] || continue # no package at or above it
		if [ "$d" = "." ]; then hit[$mod]=1; else hit[$mod/$d]=1; fi ;;
	esac
done <<<"$changed"

pkgs=()
if [ "$all" = 1 ]; then
	pkgs=(./...)
elif [ "${#hit[@]}" -gt 0 ]; then
	# -test lists each package's test variants, whose Deps hold what its tests
	# import too. "p [p.test]", "p_test [p.test]" (the external tests) and
	# "p.test" all name p.
	declare -A pick=()
	while read -r p deps; do
		p=${p%.test}; p=${p%_test}
		[ -n "${pick[$p]:-}" ] && continue
		for q in $p $deps; do
			q=${q%% *}
			if [ -n "${hit[$q]:-}" ]; then pick[$p]=1; break; fi
		done
	done < <(go list -test -f '{{.ImportPath}}	{{join .Deps " "}}' ./... | sed 's/ \[[^]]*\]//; s/	/ /')
	for p in "${!pick[@]}"; do
		case "$p" in "$mod" | "$mod"/*) pkgs+=("$p") ;; esac
	done
fi

status=0
if [ "${#pkgs[@]}" -gt 0 ]; then
	mapfile -t pkgs < <(printf '%s\n' "${pkgs[@]}" | sort)
	echo "test-changed: ${#pkgs[@]} package(s) vs $BASE"
	printf '  %s\n' "${pkgs[@]#"$mod"/}"
	[ -n "${LIST:-}" ] || go test "$@" "${pkgs[@]}" || status=$?
fi
for m in $(printf '%s\n' "${other[@]}" | sort -u); do
	echo "test-changed: $m (its own module)"
	[ -n "${LIST:-}" ] || (cd "$m" && go vet ./... && go test "$@" ./...) || status=$?
done
if [ "${#pkgs[@]}" -eq 0 ] && [ "${#other[@]}" -eq 0 ]; then
	echo "test-changed: no Go package changed vs $BASE"
fi
exit $status
