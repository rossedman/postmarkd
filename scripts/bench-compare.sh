#!/usr/bin/env bash
# Compare two benchmark runs and fail on regressions.
#
#   scripts/bench-compare.sh old.txt new.txt
#
# allocs/op is deterministic, so any statistically significant increase
# fails. Timings on shared CI machines are noisy, so sec/op only fails when
# it is significantly worse by more than MAX_SLOWDOWN percent (default 25).
set -euo pipefail

old=$1 new=$2
max_slowdown=${MAX_SLOWDOWN:-25}
benchstat=${BENCHSTAT:-benchstat}

"$benchstat" "$old" "$new"

fail=0
check() { # unit threshold
	"$benchstat" -format csv -filter ".unit:$1" "$old" "$new" 2>/dev/null |
		awk -F, -v unit="$1" -v max="$2" '
			$1 ~ /^Benchmark|^[A-Z]/ && $(NF-1) ~ /^\+[0-9.]+%$/ {
				pct = substr($(NF-1), 2) + 0
				if (pct > max) { printf "REGRESSION %-55s %s %s\n", $1, unit, $(NF-1); bad = 1 }
			}
			END { exit bad }'
}
echo
check allocs/op 0 || fail=1
check sec/op "$max_slowdown" || fail=1
if [ "$fail" = 1 ]; then
	echo "Benchmarks regressed (see above)."
	exit 1
fi
echo "No benchmark regressions."
