#!/bin/sh
# End-to-end test of tm-trace on a cluster: start, status, analyze (every
# format, one format and model, a custom format, a failure), the Tensormesh
# Platform path with a stand-in platform collector, stop (also in a namespace
# that existed before start), and a user with rights in one namespace only.
# Usage: test/tool-test.sh IMAGE TM_TRACE   (IMAGE must be available to the nodes)
set -eu

image=$1
tool=$(cd "$(dirname "$2")" && pwd)/$(basename "$2")
root=$(cd "$(dirname "$0")/.." && pwd)
work=$(mktemp -d)
ns=tm-trace-test
pns=tm-trace-platform
own=tm-trace-existing
prj=tm-trace-project

finish() {
    status=$?
    if [ "$status" -ne 0 ]; then
        echo "tool test failed (exit $status). Cluster state:"
        for n in "$ns" "$pns" "$own" "$prj"; do
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

# 3. status: the collector's counters
"$tool" status -n "$ns" | tee status.txt
grep -q "300 spans (LMCache) and 300 log records (NVIDIA Dynamo)" status.txt
echo "ok status"

# 4. analyze: every format with records, then one format and model, then custom
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

# 5. A failing analysis ends with a clear error
if analyze -n "$ns" --format lmcache --model none > fail.txt 2>&1; then
    echo "FAIL: exit 0 for a model without records"; exit 1
fi
grep -q "no LMCache trace records for model none" fail.txt
echo "ok failure"

# 6. Tensormesh Platform path, with a stand-in for the platform collector
kubectl create namespace "$pns"
sed '/^---$/q' "$root/deploy/collector/trace-collector.yaml" | kubectl -n "$pns" apply -f -
kubectl -n "$pns" apply -f "$root/test/platform-standin.yaml"
kubectl -n "$pns" rollout status deployment/tensormesh-platform-otel-collector-collector --timeout=180s
kubectl -n "$pns" create configmap trace-loader --from-file="$root/test/loader.py"
load "$pns" lmcache http://tensormesh-platform-otel-collector-collector:4318
sleep 10
"$tool" status -n "$pns" | tee platform-status.txt
grep -q "^Tensormesh Platform collector in namespace $pns" platform-status.txt
grep -q " 300 spans (LMCache)" platform-status.txt
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

# 7. stop: namespace and volume deleted
pv=$(kubectl -n "$ns" get pvc trace-collector-data -o jsonpath='{.spec.volumeName}')
"$tool" stop -n "$ns" --yes
if kubectl get namespace "$ns" > /dev/null 2>&1 || kubectl get pv "$pv" > /dev/null 2>&1; then
    echo "FAIL: namespace or volume left"; exit 1
fi
echo "ok stop"

# 8. stop in a namespace that existed before start: the namespace and its other
# objects stay, the collector and its volume go
kubectl create namespace "$own"
kubectl -n "$own" create configmap keep-me --from-literal=keep=yes
"$tool" start -n "$own" --max-size 5Gi > own-start.txt
pv=$(kubectl -n "$own" get pvc trace-collector-data -o jsonpath='{.spec.volumeName}')
"$tool" stop -n "$own" --yes > own-stop.txt
cat own-stop.txt
grep -q "Namespace $own stays" own-stop.txt
kubectl -n "$own" get configmap keep-me > /dev/null
left=$(kubectl -n "$own" get deploy,svc,cm,pvc -l app.kubernetes.io/name=trace-collector -o name)
if [ -n "$left" ] || kubectl get pv "$pv" > /dev/null 2>&1; then
    echo "FAIL: collector objects or volume left: $left"; exit 1
fi
kubectl delete namespace "$own" --timeout=5m
echo "ok stop in an existing namespace"

# 9. A user with rights in one namespace only, as on OpenShift, on a cluster
# with OpenShift's API group: no fixed user and group ids, and stop without
# permission to change volumes
kubectl apply -f "$root/test/openshift-standin.yaml"
kubectl wait --for=condition=established crd/securitycontextconstraints.security.openshift.io --timeout=60s
kubectl create namespace "$prj"
kubectl -n "$prj" create serviceaccount tm-user
kubectl -n "$prj" create rolebinding tm-user --clusterrole=admin --serviceaccount="$prj:tm-user"
user=$work/user.kubeconfig
kubectl config view --minify --raw -o jsonpath='{.clusters[0].cluster.certificate-authority-data}' | base64 -d > "$work/ca.crt"
KUBECONFIG=$user kubectl config set-cluster c --server="$(kubectl config view --minify -o jsonpath='{.clusters[0].cluster.server}')" \
    --certificate-authority="$work/ca.crt" --embed-certs > /dev/null
KUBECONFIG=$user kubectl config set-credentials u --token="$(kubectl -n "$prj" create token tm-user --duration 1h)" > /dev/null
KUBECONFIG=$user kubectl config set-context c --cluster=c --user=u > /dev/null
KUBECONFIG=$user kubectl config use-context c > /dev/null
as_user() { KUBECONFIG=$user "$tool" "$@"; }
as_user start -n "$prj" --max-size 5Gi
ids=$(kubectl -n "$prj" get deploy trace-collector -o jsonpath='{.spec.template.spec.securityContext.runAsUser}{.spec.template.spec.securityContext.runAsGroup}{.spec.template.spec.securityContext.fsGroup}')
if [ -n "$ids" ] || as_user analyze -n "$prj" --format lmcache --dry-run | grep -q -e runAsUser -e runAsGroup -e fsGroup; then
    echo "FAIL: fixed user or group ids on OpenShift"; exit 1
fi
kubectl -n "$prj" create configmap trace-loader --from-file="$root/test/loader.py"
load "$prj" lmcache http://trace-collector:4318
sleep 10
rm -f report-*.html
as_user analyze -n "$prj" --format lmcache --image "$image" --pull missing
python3 "$root/test/check_report.py" report-lmcache.html "$root/test/expected-lmcache.json"
pv=$(kubectl -n "$prj" get pvc trace-collector-data -o jsonpath='{.spec.volumeName}')
as_user stop -n "$prj" --yes > prj-stop.txt
cat prj-stop.txt
grep -q "No permission to change volume $pv\." prj-stop.txt
grep -q "Deleted the trace collector and its volume claim. Namespace $prj stays." prj-stop.txt
kubectl wait --for=delete "pv/$pv" --timeout=2m
kubectl delete namespace "$prj" --timeout=5m
kubectl delete -f "$root/test/openshift-standin.yaml"
echo "ok one-namespace user on OpenShift stand-in"
