#!/bin/sh
# matrix.sh runs the simulation matrix of bench/README.md.
#
#   WORK=DIR GITLET=BIN ./matrix.sh list            print one run per line
#   WORK=DIR GITLET=BIN ./matrix.sh run [FILTER]    run them (skipping finished ones)
#
# Runs whose name matches FILTER (a grep pattern) are run; gitlet runs go two
# at a time, as they are paced by wall time, the others one at a time.
set -eu

: "${WORK:?set WORK to the work directory}"
: "${GITLET:=gitlet}"
BENCH=${BENCH:-$(dirname "$0")/gitlet-bench}

list() {
	for profile in spread clustered hot; do
		for seed in 1 2 3; do
			for n in 4 8 16; do
				for mode in git gitsync filelock gitlet; do
					echo "$profile $seed $n $mode -"
				done
			done
		done
	done
	# Sensitivity of the gitlet result to the agents' policy.
	for profile in clustered hot; do
		for seed in 1 2 3; do
			echo "$profile $seed 8 gitlet ignore"
		done
	done
	for seed in 1 2 3; do
		echo "hot $seed 8 gitlet half"
		echo "hot $seed 8 gitlet actdup"
	done
}

one() { # one PROFILE SEED N MODE VARIANT
	name="$1-s$2-n$3-$4"
	extra=""
	case $5 in
	ignore) name="$name-ignore" extra="--overlap ignore" ;;
	half) name="$name-half" extra="--compliance 0.5" ;;
	actdup) name="$name-actdup" extra="--dup-judge act" ;;
	esac
	[ ! -s "$WORK/runs/$name/result.json" ] || return 0
	# Each run gets its own build cache, hard-linked from a warm one, so
	# that runs neither share nor grow one cache without bound.
	cache=$WORK/runs/$name.gocache
	rm -rf "$cache"
	cp -al "$WORK/gocache-base" "$cache"
	# shellcheck disable=SC2086
	"$BENCH" run --work "$WORK" --gocache "$cache" --target x --tasks "$WORK/tasks-$1-$2.json" --limit $(($3 * 4)) \
		--mode "$4" --agents "$3" --seed "$2" --gitlet "$GITLET" --name "$name" $extra \
		>"$WORK/runs/$name.log" 2>&1 || echo "failed: $name" >&2
	rm -rf "$cache"
}

case ${1:-} in
list) list ;;
run)
	mkdir -p "$WORK/runs"
	if [ ! -d "$WORK/gocache-base" ]; then
		(cd "$WORK/fixture" && GOCACHE=$WORK/gocache-base GOFLAGS= go vet -trimpath ./... && GOCACHE=$WORK/gocache-base GOFLAGS= go build -trimpath ./...)
	fi
	filter=${2:-.}
	export WORK GITLET BENCH
	list | grep -e "$filter" | grep ' gitlet ' | xargs -r -P 2 -L 1 sh -c 'shift; "$0" one "$@"' "$0" _ &
	list | grep -e "$filter" | grep -v ' gitlet ' | xargs -r -P 1 -L 1 sh -c 'shift; "$0" one "$@"' "$0" _ || true
	wait
	;;
one) shift; one "$@" ;;
*) echo "usage: matrix.sh list | run [FILTER]" >&2; exit 2 ;;
esac
