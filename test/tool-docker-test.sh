#!/bin/sh
# Test of tm-trace without Kubernetes: start (its free space check and
# --duration), status, analyze (every format, custom), and stop with Docker or
# Podman, as the current user.
# Usage: test/tool-docker-test.sh IMAGE TM_TRACE [docker|podman]
set -eu

image=$1
tool=$(cd "$(dirname "$2")" && pwd)/$(basename "$2")
runtime=${3:-docker}
root=$(cd "$(dirname "$0")/.." && pwd)
work=$(mktemp -d)

finish() {
    status=$?
    if [ "$status" -ne 0 ]; then
        echo "tool Docker test failed (exit $status). Collector log:"
        "$runtime" logs trace-collector || true
    fi
    "$runtime" rm -f trace-collector > /dev/null 2>&1 || true
    rm -rf "$work"
}
trap finish EXIT
cd "$work"

# A --max-size above the free disk space is refused before anything is created.
if "$tool" start --runtime "$runtime" --max-size 100Ti > refused.txt 2>&1; then
    echo "FAIL: start accepted --max-size 100Ti"; exit 1
fi
cat refused.txt
grep -q "less than --max-size" refused.txt
if [ -e tm-trace ] || "$runtime" inspect trace-collector > /dev/null 2>&1; then
    echo "FAIL: the refused start left files or a container"; exit 1
fi
echo "ok start: refused without enough free space"

# --duration: after the deadline, the collector writes no more trace files.
"$tool" start --runtime "$runtime" --dir timed --max-size 5Gi --duration 1s
sleep 2
python3 "$root/test/loader.py" --endpoint http://127.0.0.1:4318 --format both
sleep 5
if [ -e timed/traces/request-trace.jsonl ]; then
    echo "FAIL: trace files written after the deadline"; exit 1
fi
"$tool" status --runtime "$runtime" --dir timed | tee status.txt
grep -q "^Collection stopped at " status.txt
"$tool" stop --runtime "$runtime" --dir timed --yes
echo "ok start: --duration stops the collection"

"$tool" start --runtime "$runtime" --max-size 5Gi
grep -q "max_backups: 8$" tm-trace/collector-config.yaml
python3 "$root/test/loader.py" --endpoint http://127.0.0.1:4318 --format both
sleep 5
[ "$(stat -c %u tm-trace/traces/request-trace.jsonl)" = "$(id -u)" ]
echo "ok start: trace files owned by the current user"

"$tool" status --runtime "$runtime" | tee status.txt
grep -q "^Trace collector ($runtime): running" status.txt
grep -q "^Trace files in tm-trace/traces: [0-9.]* MiB in [1-9][0-9]* file" status.txt
echo "ok status"

"$tool" analyze --runtime "$runtime" --image "$image" --pull never
python3 "$root/test/check_report.py" report-lmcache.html "$root/test/expected-lmcache.json"
python3 "$root/test/check_report.py" report-dynamo.html "$root/test/expected-dynamo.json"
"$tool" analyze --runtime "$runtime" --image "$image" --pull never --custom-format "$root/test/custom_dynamo.py"
python3 "$root/test/check_report.py" report-custom.html "$root/test/expected-dynamo.json"
echo "ok analyze"

"$tool" stop --runtime "$runtime" --yes
if [ -e tm-trace ] || "$runtime" inspect trace-collector > /dev/null 2>&1; then
    echo "FAIL: collector or trace files left"; exit 1
fi
echo "ok stop"
