#!/usr/bin/env bash
# Stream runner pressure to the Actions log, which survives loss of the runner.
set -euo pipefail

sample() {
  printf 'TWIN_RESOURCE time=%s\n' "$(date -u +%FT%TZ)"
  free -m
  df -Pm "$RUNNER_TEMP" "$GITHUB_WORKSPACE"
  ps -eo rss=,args= | awk '
    /bazel\([^)]*\)/ && /java/ { rss += $1 }
    END { printf "TWIN_RESOURCE bazel_jvm_rss_kib=%d\n", rss }
  '
}

printf 'TWIN_RESOURCE cpus=%s\n' "$(nproc)"
trap 'exit 0' TERM INT
while :; do
  sample
  sleep 15 &
  wait "$!"
done
