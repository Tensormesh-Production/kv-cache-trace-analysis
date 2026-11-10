"""Test adapter for FORMAT=custom: reads Dynamo records with LMCache's adapter,
so the report must equal the dynamo report."""

import sys

from lmcache.cli.main import main
from lmcache.tools.cache_simulator.trace_analyze import (
    TRACE_FORMATS,
    TraceFormat,
    _dynamo_records,
)

TRACE_FORMATS["custom"] = TraceFormat(read=_dynamo_records, expects="dynamo records")

if __name__ == "__main__":
    sys.exit(main())
