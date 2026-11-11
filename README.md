# KV Cache Trace Analysis

Measures how much of an LLM workload a KV cache can reuse. A collector stores request traces in the cluster, and an analyzer Job runs the [LMCache](https://github.com/LMCache/LMCache) trace analyzer on them. The result is an HTML report of prefix and non-prefix hit rates for a range of cache sizes.

The analysis needs no GPU, no LMCache installation, and no credentials. `tm-trace` runs it with one command per step, on Kubernetes or with Docker.

## Requirements

- A Kubernetes cluster, with permission to create a namespace and to update PersistentVolumes, or a host with Docker or Podman.
- Nodes or hosts that can pull the analyzer and OpenTelemetry Collector images from `ghcr.io`. Without internet access, copy both images to a local registry, and use `--image` and `--collector-image` (or change `image` in the manifests).
- On Kubernetes, a default StorageClass for a 50 GiB `ReadWriteOnce` volume. `--max-size` and `--storage-class` change it.
- A trace source:
  - NVIDIA Dynamo 1.4 or later, or
  - LMCache 0.5.7 or later in multiprocess (MP) mode, or
  - another router that exports OpenTelemetry (OTLP) records, with a [custom trace format](#custom-trace-formats).
- Network access from the trace source to the collector on port 4317.

## Quickstart

Install `tm-trace` on macOS or Linux. On Windows, download the zip from the [latest release](https://github.com/tensormesh-production/kv-cache-trace-analysis/releases/latest).

```bash
curl -sSfL https://github.com/tensormesh-production/kv-cache-trace-analysis/releases/latest/download/install.sh | sh
```

It uses the current kubectl context. `--context` and `--kubeconfig` pick another.

1. Deploy the collector. It prints the settings for the trace source.

   ```bash
   tm-trace start
   ```

2. Set them on NVIDIA Dynamo or LMCache, and let it collect traffic.
3. Run the analysis. It saves one report per trace format, such as `report-lmcache.html`.

   ```bash
   tm-trace analyze
   ```

4. Delete the collector and the trace files.

   ```bash
   tm-trace stop
   ```

- With the Tensormesh Platform, `tm-trace` finds it in namespace `tensormesh-platform` (`-n` for another). `start` and `stop` print the settings and the helm command that turn its request traces on and off, and `analyze` uses its collector.
- Without Kubernetes, add `--docker` to each command. It uses Docker or Podman on this host, and keeps the trace files in `tm-trace/traces`.
- The trace files stay under `--max-size` (default 50 GiB). On Kubernetes, it is the volume size. With `--docker`, `start` checks that the disk has that much free space.
- `tm-trace <command> --help` lists the options, such as `--model`, `--custom-format`, `--image` for mirrors, and `--dry-run` to see what would run.

## Manual steps with kubectl

### 1. Deploy the collector

```bash
kubectl create namespace trace-analysis
kubectl -n trace-analysis apply -f https://github.com/tensormesh-production/kv-cache-trace-analysis/releases/latest/download/trace-collector.yaml
kubectl -n trace-analysis rollout status deployment/trace-collector --timeout=180s
```

The collector receives OTLP on ports 4317 (gRPC) and 4318 (HTTP), keeps only request records, and writes them to its volume. It keeps the newest 80 files of 512 MB each.

### 2. Send traces

NVIDIA Dynamo: set these environment variables on the frontend container.

```yaml
- name: DYN_REQUEST_TRACE
  value: "1"
- name: DYN_REQUEST_TRACE_RECORDS
  value: request_payload
- name: DYN_REQUEST_TRACE_SINKS
  value: otel
- name: OTEL_EXPORTER_OTLP_LOGS_ENDPOINT
  value: http://trace-collector.trace-analysis.svc:4317
- name: OTEL_BLRP_MAX_EXPORT_BATCH_SIZE
  value: "8"
```

LMCache: add these flags to the `lmcache server` command, or to `lmcache.mp.autostart.server_args` when vLLM starts the server.

```text
--enable-token-trace
--otlp-endpoint http://trace-collector.trace-analysis.svc:4317
```

With `--otlp-endpoint`, LMCache sends its metrics to the collector instead of serving `/metrics`. The collector serves them for Prometheus on port 8889. To keep the metrics on an existing collector, see [Existing OpenTelemetry Collector](#existing-opentelemetry-collector). LMCache does not trace requests shorter than one chunk (256 tokens by default).

### 3. Run the analysis

After the collection period:

```bash
FORMAT=dynamo
kubectl -n trace-analysis apply -f https://github.com/tensormesh-production/kv-cache-trace-analysis/releases/latest/download/analyzer-$FORMAT.yaml
kubectl -n trace-analysis wait job/trace-analyzer-$FORMAT --for=jsonpath='{.status.conditions[0].status}'=True --timeout=1h
kubectl -n trace-analysis logs job/trace-analyzer-$FORMAT > report-$FORMAT.html
```

- Open the report in a browser: `open report-$FORMAT.html` on macOS, `xdg-open report-$FORMAT.html` on Linux.
- For LMCache traces, set `FORMAT=lmcache`.
- `wait` returns when the Job ends, whether it succeeded or failed.
- The Job and its log are deleted one hour after the Job ends. To run the analysis again before that, delete the Job first: `kubectl -n trace-analysis delete job trace-analyzer-$FORMAT`.

### 4. Clean up

Trace files hold request content. Delete them after the analysis:

1. Remove the Dynamo environment variables or the LMCache flags.
2. Delete the namespace. The first command sets the reclaim policy of the volume to `Delete`, so that the stored data is deleted with the namespace.

```bash
kubectl patch pv "$(kubectl -n trace-analysis get pvc trace-collector-data -o jsonpath='{.spec.volumeName}')" -p '{"spec":{"persistentVolumeReclaimPolicy":"Delete"}}'
kubectl delete namespace trace-analysis
```

## Manual steps with Docker

Without Kubernetes, the collector and the analyzer run as containers on one host. Both run as the current user, so the trace files in `traces/` belong to that user. The trace files take up to 41 GiB of the host's disk (the newest 80 files of 512 MB): check the free space with `df -h .` first, and for a smaller disk, lower `max_backups` in `collector-config.yaml`.

1. Start the collector, then check that `docker logs trace-collector` ends with `Everything is ready`:

   ```bash
   curl -fsSLO https://github.com/tensormesh-production/kv-cache-trace-analysis/releases/latest/download/collector-config.yaml
   mkdir -m 700 traces
   docker run -d --name trace-collector --restart unless-stopped --user "$(id -u):$(id -g)" --cpus 2 --memory 2g \
     -p 4317:4317 -p 4318:4318 -p 8889:8889 \
     -v "$PWD/collector-config.yaml:/etc/otelcol-contrib/config.yaml:ro" -v "$PWD/traces:/data" \
     ghcr.io/open-telemetry/opentelemetry-collector-releases/opentelemetry-collector-contrib:0.159.0
   ```

2. Send traces with the settings in [step 2](#2-send-traces), with `http://<collector-host>:4317` as the endpoint.

3. After the collection period, run the analysis in the same folder:

   ```bash
   FORMAT=dynamo
   docker run --rm --pull always --network none --user "$(id -u):$(id -g)" --cpus 2 --memory 16g \
     -v "$PWD/traces:/data:ro" -e TRACE_FORMAT=$FORMAT \
     ghcr.io/tensormesh-production/kv-cache-trace-analysis/analyzer > report-$FORMAT.html
   ```

   - For analyzer options, add `-e ANALYZER_ARGS="--model <name>"`.
   - For a custom format, set `FORMAT=custom` and add `-v "$PWD/custom_format.py:/formats/custom_format.py:ro"`.

4. Clean up: remove the source settings, then delete the collector and the trace files.

   ```bash
   docker rm -f trace-collector
   rm -rf traces
   ```

## Existing OpenTelemetry Collector

The trace collector runs next to an existing collector and receives only request traces.

- Dynamo: the settings in step 2 send only the request traces to the trace collector.
- LMCache: add `--enable-token-trace`, keep `--otlp-endpoint` pointed at the existing collector, and add [`forward-request-traces.yaml`](deploy/collector/forward-request-traces.yaml) to its configuration, for example as a second `--config` file. It forwards only the request trace spans; the existing pipelines are unchanged.

## Report

`report-<format>.html` opens offline in any browser. For each cache size and chunk size, it shows:

- Prefix hit rate: share of tokens in the leading run of cached chunks of each request.
- Non-prefix hit rate: share of tokens that CacheBlend could reuse outside that prefix.
- Request and token totals, and LRU evictions.

A KV cache is not shared between models. If the report lists several models, run one analysis per model with `--model` (see [Analyzer options](#analyzer-options)).

## Analyzer options

With `tm-trace`, use `--model` and `--analyzer-args`. In the manual steps, `ANALYZER_ARGS` in the Job manifest passes options to `lmcache bench trace-analyze`, such as `--model <name>`, `--chunk-size`, and `--capacity`. Download the manifest, set the value, then apply the file:

```bash
FORMAT=dynamo
curl -fsSLO https://github.com/tensormesh-production/kv-cache-trace-analysis/releases/latest/download/analyzer-$FORMAT.yaml
kubectl -n trace-analysis apply -f analyzer-$FORMAT.yaml
```

## Custom trace formats

For other trace sources, implement `read()` in `custom_format.py`. The file describes its input and output. Then run `tm-trace analyze --custom-format custom_format.py`. In the manual steps, load it into the cluster, then run the analysis with `FORMAT=custom`:

```bash
curl -fsSLO https://github.com/tensormesh-production/kv-cache-trace-analysis/releases/latest/download/custom_format.py
kubectl -n trace-analysis create configmap trace-format --from-file=custom_format.py
```

- The source must send one span or log record per request that holds the request input.
- Routers that use the OpenTelemetry SDK keep at most 128 attributes per span by default. Set `OTEL_SPAN_ATTRIBUTE_COUNT_LIMIT=4096` on the router so that spans keep the request input.
- Check that the request count in the report matches the router's.

## Sizing

The analyzer needs about 0.4 GB of memory plus 38 MB per million tokens, and takes about 3 seconds per million tokens. The Job's 16 GiB memory limit covers about 400 million tokens: about 1.8 GB of Dynamo trace files, or 9 GB of LMCache trace files. For larger traces, raise the memory limit (`--memory` with `tm-trace` or Docker, `resources.limits.memory` in the Job manifest), or analyze one model at a time.

## Privacy and security

- Trace files hold request content: request payloads for Dynamo, token IDs for LMCache. They stay on the collector's volume, or in `traces/` with Docker. No component sends data out of the cluster or the host.
- The analyzer mounts the volume read-only and makes no network connections.
- Both pods run as a non-root user with a read-only root filesystem, have no Kubernetes API token, and meet the Pod Security `restricted` profile.
- With Docker, the collector's ports are open on all host interfaces. To accept traces only from the same host, publish them on `127.0.0.1`, for example `-p 127.0.0.1:4317:4317`.
- The report holds aggregate numbers and model names only.
- `tm-trace` applies these same manifests. It changes nothing outside its namespace, except the trace volume's reclaim policy at `stop`, and never changes a Tensormesh Platform release.
- Releases include `checksums.txt`, signed with cosign by the release workflow. The installer checks the checksum, and the signature when cosign is installed.

## Troubleshooting

| Symptom | Fix |
|---|---|
| The report file shows `No '<format>' trace records` | Check `FORMAT`. Then check the source settings, and that no NetworkPolicy blocks port 4317 to the collector. |
| The report file shows a JSON error | The newest trace file was still being written. Delete the Job and run it again. |
| `wait` does not return and the analyzer pod is `Pending` | The analyzer runs on the collector's node, because the volume is `ReadWriteOnce`. Free 1 CPU and 1 GiB of memory on that node. |
| The report file is empty | Run `kubectl -n trace-analysis get pods`. For `OOMKilled`, see [Sizing](#sizing). |

## Image

`ghcr.io/tensormesh-production/kv-cache-trace-analysis/analyzer`, for `linux/amd64` and `linux/arm64`:

- Python 3.12 slim, the LMCache wheel, and only the packages the analyzer imports, with CPU-only PyTorch. About 0.4 GB compressed.
- Tags: `latest` and the LMCache version, such as `0.5.7`. New stable LMCache releases are checked daily, then built, tested, and published automatically. A release that fails a test is not published.
- Image tags follow LMCache versions; release versions, such as `v0.1.0`, are for `tm-trace` and the manifests. To use one LMCache version, pass `--image` with that tag, or set `image` in the Job manifest.
- Built from [`image/`](image/) with hash-pinned requirements.

## License

[Apache License 2.0](LICENSE)
