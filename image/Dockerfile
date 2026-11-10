# syntax=docker/dockerfile:1
# Analyzer image: the LMCache wheel without its serving dependencies, plus only
# the packages the trace analyzer imports, with CPU-only PyTorch.
# Docker official image, from its Amazon ECR Public mirror (no Docker Hub pull limit).
FROM public.ecr.aws/docker/library/python:3.12-slim-bookworm

# LMCache release to install, such as 0.5.7 (set by CI).
ARG LMCACHE_VERSION

LABEL org.opencontainers.image.source="https://github.com/Tensormesh-Production/kv-cache-trace-analysis" \
      org.opencontainers.image.description="LMCache trace analyzer for KV cache hit rates, CPU only" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.version="${LMCACHE_VERSION}"

COPY requirements.txt /tmp/requirements.txt
# The pip_cert secret is optional: a CA bundle for builds behind a TLS-inspecting proxy.
RUN --mount=type=secret,id=pip_cert,target=/run/secrets/pip_cert \
    set -eux; \
    test -n "${LMCACHE_VERSION}"; \
    if [ -s /run/secrets/pip_cert ]; then export PIP_CERT=/run/secrets/pip_cert; fi; \
    pip install --no-cache-dir --no-compile --no-deps --require-hashes -r /tmp/requirements.txt \
        --index-url https://download.pytorch.org/whl/cpu --extra-index-url https://pypi.org/simple; \
    pip install --no-cache-dir --no-compile --no-deps --only-binary :all: "lmcache==${LMCACHE_VERSION}"; \
    rm /tmp/requirements.txt; \
    python -B -c "import lmcache.tools.cache_simulator.trace_analyze"

COPY --chmod=0755 analyze.sh /usr/local/bin/analyze

ENV HOME=/tmp \
    LMCACHE_DISABLE_BANNER=1 \
    TRACE_FORMAT=dynamo \
    ANALYZER_ARGS=""
USER 10001:10001
WORKDIR /tmp
CMD ["analyze"]
