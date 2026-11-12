#!/bin/sh
# Docker test with the README commands and the local files: the collector writes
# synthetic traces to a host folder as the current user, the analyzer reads them
# with no network, and the folder is deleted without root.
# Usage: test/docker-test.sh IMAGE
set -eu

image=$1
root=$(cd "$(dirname "$0")/.." && pwd)
work=$(mktemp -d)

# On failure, print the collector log so the CI log shows the cause.
finish() {
    status=$?
    if [ "$status" -ne 0 ]; then
        echo "Docker test failed (exit $status). Collector log:"
        docker logs trace-collector || true
    fi
    docker rm -f trace-collector > /dev/null 2>&1 || true
    rm -rf "$work"
}
trap finish EXIT

# 1. The Docker configuration is the configuration in the Kubernetes ConfigMap
sed -n '/^  config.yaml: |$/,/^---$/p' "$root/deploy/collector/trace-collector.yaml" |
    sed -e '1d' -e '$d' -e 's/^    //' > "$work/configmap.yaml"
grep -v '^#' "$root/deploy/docker/collector-config.yaml" | diff -u "$work/configmap.yaml" -
echo "ok Docker configuration matches the ConfigMap"

# 2. Collector
cd "$work"
cp "$root/deploy/docker/collector-config.yaml" .
mkdir -m 700 traces
docker run -d --name trace-collector --restart unless-stopped --user "$(id -u):$(id -g)" --cpus 2 --memory 2g \
    -p 4317:4317 -p 4318:4318 -p 8889:8889 \
    -v "$PWD/collector-config.yaml:/etc/otelcol-contrib/config.yaml:ro" -v "$PWD/traces:/data" \
    ghcr.io/open-telemetry/opentelemetry-collector-releases/opentelemetry-collector-contrib:0.159.0

# 3. Synthetic traces over OTLP
python3 "$root/test/loader.py" --endpoint http://127.0.0.1:4318 --format both
sleep 5
owner=$(stat -c %u traces/request-trace.jsonl)
if [ "$owner" != "$(id -u)" ]; then echo "FAIL: trace file owner $owner"; exit 1; fi
echo "ok trace files owned by the current user"

# 4. Analysis. The README also passes --pull always; the test image is local only.
# analyze FORMAT [docker run options]: prints the analyzer output
analyze() {
    format=$1
    shift
    docker run --rm --network none --user "$(id -u):$(id -g)" --cpus 2 --memory 16g \
        -v "$PWD/traces:/data:ro" -e TRACE_FORMAT="$format" "$@" "$image"
}
analyze dynamo > report-dynamo.html
python3 "$root/test/check_report.py" report-dynamo.html "$root/test/expected-dynamo.json"
analyze lmcache > report-lmcache.html
python3 "$root/test/check_report.py" report-lmcache.html "$root/test/expected-lmcache.json"
cp "$root/test/custom_dynamo.py" custom_format.py
analyze custom -v "$PWD/custom_format.py:/formats/custom_format.py:ro" > report-custom.html
python3 "$root/test/check_report.py" report-custom.html "$root/test/expected-dynamo.json"

# 5. Clean up
docker rm -f trace-collector
rm -rf traces
echo "ok clean up: collector and trace files deleted"
