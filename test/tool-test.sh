#!/bin/sh
# End-to-end test of tm-trace on a cluster: start, analyze (every format, one
# format and model, a custom format, a failure), the Tensormesh Platform path
# with a stand-in platform collector, and stop.
# Usage: test/tool-test.sh IMAGE TM_TRACE   (IMAGE must be available to the nodes)
set -eu

image=$1
tool=$(cd "$(dirname "$2")" && pwd)/$(basename "$2")
root=$(cd "$(dirname "$0")/.." && pwd)
work=$(mktemp -d)
ns=tm-trace-test
pns=tm-trace-platform

finish() {
    status=$?
    if [ "$status" -ne 0 ]; then
        echo "tool test failed (exit $status). Cluster state:"
        for n in "$ns" "$pns"; do
            kubectl -n "$n" get all,pvc -o wide || true
            kubectl -n "$n" get events --sort-by=.lastTimestamp || true
        done
    fi
    rm -rf "$work"
}
trap finish EXIT

analyze() { "$tool" analyze --image "$image" --pull missing "$@"; }

load() {
    sed -e "s#__FORMAT__#$2#g" -e "s#__ENDPOINT__#$3#" -e "s#__IMAGE__#$image#" \
        "$root/test/loader-job.yaml" | kubectl -n "$1" apply -f -
    kubectl -n "$1" wait "job/trace-loader-$2" --for=condition=complete --timeout=5m
}

cd "$work"

# 1. start: collector ready, settings printed
"$tool" start -n "$ns" --max-size 5Gi > start.txt
cat start.txt
grep -q -- "--otlp-endpoint http://trace-collector.$ns.svc:4317" start.txt
kubectl -n "$ns" get configmap trace-collector -o jsonpath='{.data.config\.yaml}' | grep -q "max_backups: 8$"
echo "ok start"

# 2. Synthetic traces of both formats
kubectl -n "$ns" create configmap trace-loader --from-file="$root/test/loader.py"
load "$ns" dynamo http://trace-collector:4318
load "$ns" lmcache http://trace-collector:4318
sleep 10

# 3. analyze: every format with records, then one format and model, then custom
analyze -n "$ns"
python3 "$root/test/check_report.py" report-lmcache.html "$root/test/expected-lmcache.json"
python3 "$root/test/check_report.py" report-dynamo.html "$root/test/expected-dynamo.json"
analyze -n "$ns" --format dynamo --model model-b
grep -q model-b report-dynamo.html && ! grep -q model-a report-dynamo.html
analyze -n "$ns" --custom-format "$root/test/custom_dynamo.py"
python3 "$root/test/check_report.py" report-custom.html "$root/test/expected-dynamo.json"
if [ -n "$(kubectl -n "$ns" get jobs -l app.kubernetes.io/name=trace-analyzer -o name)" ]; then
    echo "FAIL: analyzer Jobs left"; exit 1
fi
echo "ok analyze"

# 4. A failing analysis ends with a clear error
if analyze -n "$ns" --format lmcache --model none > fail.txt 2>&1; then
    echo "FAIL: exit 0 for a model without records"; exit 1
fi
grep -q "no LMCache trace records for model none" fail.txt
echo "ok failure"

# 5. Tensormesh Platform path, with a stand-in for the platform collector
kubectl create namespace "$pns"
sed '/^---$/q' "$root/deploy/collector/trace-collector.yaml" | kubectl -n "$pns" apply -f -
kubectl -n "$pns" apply -f "$root/test/platform-standin.yaml"
kubectl -n "$pns" rollout status deployment/tensormesh-platform-otel-collector-collector --timeout=180s
kubectl -n "$pns" create configmap trace-loader --from-file="$root/test/loader.py"
load "$pns" lmcache http://tensormesh-platform-otel-collector-collector:4318
sleep 10
"$tool" start -n "$pns" > platform-start.txt
cat platform-start.txt
grep -q -- "^      - --enable-token-trace$" platform-start.txt
grep -q "helm upgrade tensormesh-platform .* --version 1.0.1 -n $pns --reuse-values -f trace-values.yaml$" platform-start.txt
rm -f report-*.html
analyze -n "$pns"
python3 "$root/test/check_report.py" report-lmcache.html "$root/test/expected-lmcache.json"
if [ -e report-dynamo.html ]; then echo "FAIL: platform path analyzed Dynamo"; exit 1; fi
pv=$(kubectl -n "$pns" get pvc tensormesh-platform-otel-collector-request-trace -o jsonpath='{.spec.volumeName}')
"$tool" stop -n "$pns" --yes > platform-stop.txt
cat platform-stop.txt
grep -q "^    extraArgs: \[\]$" platform-stop.txt
grep -q "helm upgrade tensormesh-platform .* -n $pns --reuse-values -f trace-off-values.yaml$" platform-stop.txt
[ "$(kubectl get pv "$pv" -o jsonpath='{.spec.persistentVolumeReclaimPolicy}')" = Delete ]
kubectl delete namespace "$pns" --timeout=5m
echo "ok platform"

# 6. stop: namespace and volume deleted
pv=$(kubectl -n "$ns" get pvc trace-collector-data -o jsonpath='{.spec.volumeName}')
"$tool" stop -n "$ns" --yes
if kubectl get namespace "$ns" > /dev/null 2>&1 || kubectl get pv "$pv" > /dev/null 2>&1; then
    echo "FAIL: namespace or volume left"; exit 1
fi
echo "ok stop"
