#!/usr/bin/env bash
# Drives the real psql client against a fresh basalt server and compares
# its output with the expected files.
#
#   test/psql/run.sh            run all scripts, fail on any difference
#   test/psql/run.sh --update   rewrite the expected output
#
# PSQL may point at a specific psql binary.
set -euo pipefail
cd "$(dirname "$0")/../.."
PSQL=${PSQL:-$(command -v psql || echo /opt/homebrew/opt/libpq/bin/psql)}
if ! "$PSQL" --version >/dev/null 2>&1; then
  echo "psql not found; set PSQL" >&2
  exit 2
fi
work=$(mktemp -d)
trap 'kill $pid 2>/dev/null || true; wait $pid 2>/dev/null || true; rm -rf "$work"' EXIT
go build -o "$work/basalt" ./cmd/basalt
"$work/basalt" -D "$work/data" -listen 127.0.0.1:0 -autovacuum-interval 0 >"$work/server.log" 2>&1 &
pid=$!
for _ in $(seq 100); do grep -q listening "$work/server.log" && break; sleep 0.05; done
port=$(grep -o '127.0.0.1:[0-9]*' "$work/server.log" | head -1 | cut -d: -f2)
status=0
for sql in test/psql/*.sql; do
  name=$(basename "$sql" .sql)
  out="$work/$name.out"
  PGOPTIONS= "$PSQL" -X -a -h 127.0.0.1 -p "$port" -U basalt -d basalt -v ON_ERROR_STOP=0 -f "$sql" >"$out" 2>&1 || true
  # psql prefixes errors with the script name and line; keep them stable.
  sed -i.bak -e "s#^psql:[^:]*:\([0-9]*\): #psql:$name.sql:\1: #" "$out" && rm -f "$out.bak"
  if [[ "${1:-}" == "--update" ]]; then
    cp "$out" "test/psql/$name.expected"
    echo "updated $name.expected"
  elif ! diff -u "test/psql/$name.expected" "$out"; then
    echo "FAIL: $name" >&2
    status=1
  else
    echo "ok   $name ($(grep -c . "$out") lines of psql output match)"
  fi
done
exit $status
