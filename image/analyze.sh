#!/bin/sh
# Analyzes the trace files in /data with the LMCache trace analyzer.
# On success, prints the HTML report to stdout. On failure, prints the
# analyzer output and exits 1.
#
# TRACE_FORMAT   dynamo, lmcache, or custom (adapter in /formats/custom_format.py)
# ANALYZER_ARGS  extra options for "lmcache bench trace-analyze", such as --model
set -u

format="${TRACE_FORMAT:-dynamo}"
if [ "$format" = custom ]; then
    set -- python3 /formats/custom_format.py
else
    set -- lmcache
fi

# ANALYZER_ARGS is split on spaces on purpose.
# shellcheck disable=SC2086
if "$@" bench trace-analyze -i /data/*.jsonl --trace-format "$format" \
    --html /tmp/report.html ${ANALYZER_ARGS:-} > /tmp/run.log 2>&1; then
    cat /tmp/report.html
else
    cat /tmp/run.log
    exit 1
fi
