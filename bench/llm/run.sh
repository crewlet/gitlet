#!/bin/sh
# run.sh sets up and checks one run of the agent study (see README.md):
#
#   run.sh setup DIR SCENARIO MODE   make DIR/remote.git and the clones DIR/a and DIR/b
#   run.sh prompt DIR AGENT          print the task prompt for agent a or b
#   run.sh check DIR SCENARIO        judge DIR/remote.git's main, print JSON
#
# MODE is git or gitlet. GITLET is the gitlet binary (default: gitlet on PATH).
set -eu

here=$(cd "$(dirname "$0")" && pwd)
fixture=$here/testdata/fixture
acceptance=$here/testdata/acceptance

ident() { # ident CLONE NAME
	git -C "$1" config user.name "$2"
	git -C "$1" config user.email "$2@bench.invalid"
	git -C "$1" config commit.gpgsign false
	git -C "$1" config tag.gpgsign false
	git -C "$1" config push.negotiate false
}

setup() {
	dir=$1 scenario=$2 mode=$3
	[ -d "$acceptance/$scenario" ] || { echo "unknown scenario $scenario" >&2; exit 2; }
	[ ! -e "$dir" ] || { echo "$dir exists" >&2; exit 2; }
	mkdir -p "$dir/seed"
	cp -R "$fixture/." "$dir/seed/"
	git -C "$dir/seed" init -q -b main
	ident "$dir/seed" bench
	git -C "$dir/seed" add -A
	GIT_AUTHOR_DATE=2026-01-01T00:00:00Z GIT_COMMITTER_DATE=2026-01-01T00:00:00Z \
		git -C "$dir/seed" commit -q -m "kvsvc: base"
	git init -q --bare -b main "$dir/remote.git"
	git -C "$dir/remote.git" config core.logAllRefUpdates always
	git -C "$dir/seed" push -q "$dir/remote.git" main
	rm -rf "${dir:?}/seed"
	for id in a b; do
		git clone -q "$dir/remote.git" "$dir/$id"
		ident "$dir/$id" "agent-$id"
		if [ "$mode" = gitlet ]; then
			gl=${GITLET:-$(command -v gitlet)}
			# The agent puts bin-ID first on PATH, so that `gitlet` in the
			# guide, in notes' options and in the git hooks runs as it.
			mkdir -p "$dir/bin-$id"
			cat >"$dir/bin-$id/gitlet" <<EOF
#!/bin/sh
# gitlet as agent-$id, logged to $dir/gitlet-$id.log
printf '%s start %s\n' "\$(date +%s.%N)" "\$*" >>"$dir/gitlet-$id.log"
GITLET_AGENT=agent-$id "$gl" "\$@"
code=\$?
printf '%s exit %s %s\n' "\$(date +%s.%N)" "\$code" "\$1" >>"$dir/gitlet-$id.log"
exit \$code
EOF
			chmod +x "$dir/bin-$id/gitlet"
			git -C "$dir/$id" config gitlet.snapshotInterval 5s
			# Only the git hooks: no MCP server or client hooks reach the
			# agents here, so drop the client files init writes.
			(cd "$dir/$id" && GITLET_AGENT=agent-$id "$gl" init --client claude-code --git-hooks >/dev/null)
			rm -rf "${dir:?}/$id/.mcp.json" "${dir:?}/$id/.claude"
		fi
	done
	git -C "$dir/remote.git" rev-parse main >"$dir/base"
	echo "$mode" >"$dir/mode"
	echo "$scenario" >"$dir/scenario"
}

check() {
	dir=$1 scenario=$2
	tmp=$(mktemp -d)
	trap 'rm -rf "$tmp"' EXIT
	git clone -q "$dir/remote.git" "$tmp/main"
	cd "$tmp/main"
	ok() { if "$@" >"$tmp/out" 2>&1; then echo true; else echo false; fi; }
	build=$(ok go build ./...)
	vet=$(ok go vet ./...)
	tests=$(ok go test -count=1 ./...)
	cp -R "$acceptance/$scenario/." .
	rm -f scenarios.json
	accept=$(ok go test -count=1 -run Acceptance ./...)
	acceptOut=$(tail -c 1500 "$tmp/out" | tr -c '[:print:]' ' ' | sed 's/["\\]/ /g')
	cmd=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))[sys.argv[2]]["check"])' "$acceptance/scenarios.json" "$scenario")
	extra=true
	[ -z "$cmd" ] || extra=$(ok sh -c "$cmd")
	git checkout -q -- . 2>/dev/null; git clean -qfd
	base=$(cat "$dir/base")
	commits=$(git rev-list --count "$base"..HEAD)
	merges=$(git rev-list --count --merges "$base"..HEAD)
	lines=$(git diff --shortstat "$base" HEAD)
	refs=$(git -C "$dir/remote.git" for-each-ref --format='%(refname)' | wc -l)
	printf '{"build":%s,"vet":%s,"tests":%s,"acceptance":%s,"check":%s,"commits":%s,"merges":%s,"diff":"%s","remoteRefs":%s,"acceptanceOutput":"%s"}\n' \
		"$build" "$vet" "$tests" "$accept" "$extra" "$commits" "$merges" "$(echo $lines)" "$refs" "$acceptOut"
}

case ${1:-} in
setup) shift; setup "$@" ;;
prompt) shift; python3 "$here/prompt.py" "$@" ;;
check) shift; check "$@" ;;
*) echo "usage: run.sh setup DIR SCENARIO MODE | prompt DIR AGENT | check DIR SCENARIO" >&2; exit 2 ;;
esac
