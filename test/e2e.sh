#!/bin/sh
# End-to-end test on a cluster with the README commands and the local manifests:
# Pod Security "restricted", synthetic traces over OTLP, each trace format,
# a failing Job, no ServiceAccount token, and clean up.
# Usage: test/e2e.sh IMAGE   (IMAGE must be available to the cluster's nodes)
set -eu

image=$1
root=$(cd "$(dirname "$0")/.." && pwd)
work=$(mktemp -d)
ns=trace-analysis

# On failure, print the cluster state so the CI log shows the cause.
finish() {
    status=$?
    if [ "$status" -ne 0 ]; then
        echo "e2e failed (exit $status). Cluster state:"
        kubectl -n "$ns" get all,pvc -o wide || true
        kubectl -n "$ns" describe pods || true
        kubectl -n "$ns" get events --sort-by=.lastTimestamp || true
    fi
    rm -rf "$work"
}
trap finish EXIT

# The analyzer manifest with the test image. CI runners have 2 CPUs and kind's
# control plane requests about 1, so the test lowers the analyzer's CPU request.
manifest() {
    sed -e "s#ghcr.io/tensormesh-production/kv-cache-trace-analysis/analyzer:latest#$image#" \
        -e "s#imagePullPolicy: Always#imagePullPolicy: IfNotPresent#" \
        -e 's#cpu: "1"$#cpu: 100m#' \
        "$root/deploy/analyzer/analyzer-$1.yaml" > "$work/analyzer-$1.yaml"
}

no_token() {
    volumes=$(kubectl -n "$ns" get pod -l "$1" -o jsonpath='{.items[0].spec.volumes[*].name}')
    case "$volumes" in
        *kube-api-access*) echo "FAIL: ServiceAccount token in pod $1"; exit 1 ;;
    esac
    echo "ok no ServiceAccount token: $1"
}

# run_analysis FORMAT EXPECTED: the README's analysis commands
run_analysis() {
    FORMAT=$1
    manifest "$FORMAT"
    kubectl -n "$ns" apply -f "$work/analyzer-$FORMAT.yaml"
    kubectl -n "$ns" wait "job/trace-analyzer-$FORMAT" --for=jsonpath='{.status.conditions[0].status}'=True --timeout=10m
    kubectl -n "$ns" logs "job/trace-analyzer-$FORMAT" > "$work/report-$FORMAT.html"
    python3 "$root/test/check_report.py" "$work/report-$FORMAT.html" "$2"
}

# 1. Collector, in a namespace that enforces Pod Security "restricted"
kubectl create namespace "$ns"
kubectl label namespace "$ns" pod-security.kubernetes.io/enforce=restricted pod-security.kubernetes.io/warn=restricted
kubectl -n "$ns" apply -f "$root/deploy/collector/trace-collector.yaml" 2> "$work/apply.err"
if grep -i "violate PodSecurity" "$work/apply.err"; then echo "FAIL: Pod Security warnings"; exit 1; fi
kubectl -n "$ns" rollout status deployment/trace-collector --timeout=180s
no_token app.kubernetes.io/name=trace-collector

# 2. Synthetic traces over OTLP: Dynamo straight to the trace collector, LMCache
#    through an existing collector with deploy/collector/forward-request-traces.yaml
kubectl -n "$ns" create configmap existing-collector \
    --from-file=base.yaml="$root/test/existing-base.yaml" \
    --from-file=forward.yaml="$root/deploy/collector/forward-request-traces.yaml"
kubectl -n "$ns" apply -f "$root/test/existing-collector.yaml"
kubectl -n "$ns" rollout status deployment/existing-collector --timeout=180s
kubectl -n "$ns" create configmap trace-loader --from-file="$root/test/loader.py"
load() {
    sed -e "s#__FORMAT__#$1#g" -e "s#__ENDPOINT__#$2#" -e "s#__IMAGE__#$image#" \
        "$root/test/loader-job.yaml" | kubectl -n "$ns" apply -f -
    kubectl -n "$ns" wait "job/trace-loader-$1" --for=condition=complete --timeout=5m
    kubectl -n "$ns" logs "job/trace-loader-$1"
}
load dynamo http://trace-collector:4318
load lmcache http://existing-collector:4318
sleep 10

# 3. Analysis
run_analysis dynamo "$root/test/expected-dynamo.json"
run_analysis lmcache "$root/test/expected-lmcache.json"
no_token job-name=trace-analyzer-dynamo
kubectl -n "$ns" create configmap trace-format --from-file=custom_format.py="$root/test/custom_dynamo.py"
run_analysis custom "$root/test/expected-dynamo.json"

# 4. wait also returns when the Job fails
manifest lmcache
sed -e 's/name: trace-analyzer-lmcache/name: trace-analyzer-fail/' -e 's/value: ""/value: "--model none"/' \
    "$work/analyzer-lmcache.yaml" | kubectl -n "$ns" apply -f -
kubectl -n "$ns" wait job/trace-analyzer-fail --for=jsonpath='{.status.conditions[0].status}'=True --timeout=5m
state=$(kubectl -n "$ns" get job trace-analyzer-fail -o jsonpath='{.status.conditions[*].type}')
case "$state" in
    *Failed*|*FailureTarget*) echo "ok failed Job ends the wait: $state" ;;
    *) echo "FAIL: Job conditions $state"; exit 1 ;;
esac

# 5. Clean up
pv=$(kubectl -n "$ns" get pvc trace-collector-data -o jsonpath='{.spec.volumeName}')
kubectl patch pv "$pv" -p '{"spec":{"persistentVolumeReclaimPolicy":"Delete"}}'
kubectl delete namespace "$ns" --timeout=5m
for _ in $(seq 60); do
    kubectl get pv "$pv" > /dev/null 2>&1 || break
    sleep 2
done
if kubectl get pv "$pv" > /dev/null 2>&1; then echo "FAIL: volume $pv left"; exit 1; fi
echo "ok clean up: namespace and volume deleted"
