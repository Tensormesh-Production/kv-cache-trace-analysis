"""Custom trace format for the analyzer.

Implement read() for the records of a router, then run the analysis with
FORMAT=custom (see the README).

Input of read(): one parsed line of a trace file. Each line is one OTLP JSON
export as the collector received it: {"resourceSpans": [...]} for spans or
{"resourceLogs": [...]} for log records.

Output of read(): one TraceRecord per request in that line, with
  input_ids     the request's token ids. For records that hold text, the
                UTF-8 bytes of the text the model reads (tools, system
                prompt, then each message), packed 4 per integer.
  timestamp_ns  arrival time, in nanoseconds
  request_id    any value unique to the request
  model         model name, or an empty string

The lmcache and dynamo adapters in LMCache
(lmcache/tools/cache_simulator/trace_analyze.py) are complete examples.
"""

import sys

from lmcache.cli.main import main
from lmcache.tools.cache_simulator.trace_analyze import (
    TRACE_FORMATS,
    TraceFormat,
    TraceRecord,
)


def read(line):
    """Yield a TraceRecord for each request in one parsed trace line."""
    raise NotImplementedError("custom_format.py: implement read()")
    yield TraceRecord(timestamp_ns=0, model="", input_ids=[], request_id="")


TRACE_FORMATS["custom"] = TraceFormat(read=read, expects="custom request records")

if __name__ == "__main__":
    sys.exit(main())
