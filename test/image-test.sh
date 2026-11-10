#!/bin/sh
# Golden tests for the analyzer image: each trace format, analyzer options and
# the error path, with no network, a read-only root filesystem and a non-root user.
# Usage: test/image-test.sh IMAGE
set -eu

image=$1
root=$(cd "$(dirname "$0")/.." && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
mkdir -p "$work/both" "$work/dynamo-only" "$work/out"
python3 "$root/test/loader.py" --format both --file "$work/both/trace.jsonl"
python3 "$root/test/loader.py" --format dynamo --file "$work/dynamo-only/trace.jsonl"
chmod -R a+rX "$work"

# analyze DATA_DIR FORMAT [docker run options]: prints the analyzer output
analyze() {
    data=$1
    format=$2
    shift 2
    docker run --rm --network none --read-only --tmpfs /tmp --user 10001:10001 \
        -v "$data:/data:ro" -v "$root/test/custom_dynamo.py:/formats/custom_format.py:ro" \
        -e TRACE_FORMAT="$format" "$@" "$image"
}

for format in dynamo lmcache custom; do
    analyze "$work/both" "$format" > "$work/out/$format.html"
done
python3 "$root/test/check_report.py" "$work/out/dynamo.html" "$root/test/expected-dynamo.json"
python3 "$root/test/check_report.py" "$work/out/lmcache.html" "$root/test/expected-lmcache.json"
python3 "$root/test/check_report.py" "$work/out/custom.html" "$root/test/expected-dynamo.json"

analyze "$work/both" dynamo -e ANALYZER_ARGS="--model model-b" > "$work/out/model-b.html"
if ! grep -q 'model-b' "$work/out/model-b.html" || grep -q 'model-a' "$work/out/model-b.html"; then
    echo "FAIL: ANALYZER_ARGS"
    exit 1
fi
echo "ok ANALYZER_ARGS=--model model-b"

if analyze "$work/dynamo-only" lmcache > "$work/out/none.txt"; then
    echo "FAIL: exit 0 without lmcache records"
    exit 1
fi
grep -q "No 'lmcache' trace records" "$work/out/none.txt" || { cat "$work/out/none.txt"; exit 1; }
echo "ok no records: exit 1 with the analyzer's message"
