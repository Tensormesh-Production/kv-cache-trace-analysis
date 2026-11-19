"""KServe trace format for the analyzer.

Reads the requests that KServe's payload logger (InferenceService
spec.predictor.logger, mode: request) sends to the trace collector's KServe
input. Each request arrives as a CloudEvent: the body is the request JSON, and
the collector keeps the ce-* headers as log attributes. Run the analysis with
tm-trace analyze --format kserve, or with FORMAT=custom and this file as
custom_format.py (see the README).

Reads OpenAI chat and completions requests. Other requests, and KServe's
response events, are skipped. Like LMCache's dynamo format, a text request
becomes its UTF-8 bytes packed 4 per integer (about one per token), so the
results are an estimate; a completions request with token ids is used as is.
"""

import json
import re
import struct
import sys
from datetime import datetime

from lmcache.cli.main import main
from lmcache.tools.cache_simulator.trace_analyze import (
    TRACE_FORMATS,
    TraceFormat,
    TraceRecord,
)

#: Instrumentation scope of the collector's webhook_event receiver.
SCOPE = "otlp/webhook_event"
#: CloudEvent type of a request logged by KServe.
REQUEST_TYPE = "org.kubeflow.serving.inference.request"

_RFC3339 = re.compile(r"(\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d)(?:\.(\d+))?(Z|[+-]\d\d:\d\d)")


def _value(v):
    """Python value of an OTLP JSON attribute value."""
    if "arrayValue" in v:
        return [_value(x) for x in v["arrayValue"].get("values", [])]
    for key in ("stringValue", "boolValue", "doubleValue"):
        if key in v:
            return v[key]
    if "intValue" in v:
        return int(v["intValue"])
    return None


def _header(attrs, name):
    """First value of a ce-* header kept by the collector, or None."""
    v = attrs.get("header." + name)
    if isinstance(v, list):
        return v[0] if v else None
    return v


def _time_ns(value):
    """Nanoseconds of an RFC 3339 time such as 2026-11-16T21:30:00.123456789Z."""
    m = _RFC3339.fullmatch(value or "")
    if m is None:
        return None
    zone = "+00:00" if m[3] == "Z" else m[3]
    seconds = int(datetime.fromisoformat(m[1] + zone).timestamp())
    return seconds * 10**9 + int((m[2] or "")[:9].ljust(9, "0"))


def _chat_text(request):
    """A chat request's text in prompt order: tools, then each message.

    The same text as LMCache's dynamo format, so the same requests give the
    same report through KServe as through Dynamo.
    """
    parts = []
    if request.get("tools"):
        parts.append(json.dumps(request["tools"], ensure_ascii=False, sort_keys=True))
    for message in request["messages"]:
        content = message.get("content")
        if isinstance(content, list):
            content = "".join(part.get("text", "") for part in content)
        parts.append(f"{message['role']}\n{content or ''}")
    return "\n".join(parts)


def _packed(text):
    """UTF-8 bytes of text packed 4 per integer, as LMCache's dynamo format."""
    data = text.encode("utf-8")
    data += b"\0" * (-len(data) % 4)
    return list(struct.unpack(f">{len(data) // 4}I", data))


def _prompts(request):
    """Input ids of each prompt in an OpenAI request; empty for other requests."""
    if isinstance(request.get("messages"), list):
        return [_packed(_chat_text(request))]
    prompt = request.get("prompt")
    if isinstance(prompt, str):
        return [_packed(prompt)]
    if isinstance(prompt, list) and prompt:
        if all(isinstance(p, int) for p in prompt):
            return [prompt]
        if all(isinstance(p, str) for p in prompt):
            return [_packed(p) for p in prompt]
        if all(isinstance(p, list) and all(isinstance(t, int) for t in p) for p in prompt):
            return prompt
    return []


def read(line):
    """Yield a TraceRecord for each KServe request in one parsed trace line."""
    for resource_logs in line.get("resourceLogs", []):
        for scope_logs in resource_logs.get("scopeLogs", []):
            if scope_logs.get("scope", {}).get("name") != SCOPE:
                continue
            for log in scope_logs.get("logRecords", []):
                attrs = {a["key"]: _value(a["value"]) for a in log.get("attributes", [])}
                if _header(attrs, "Ce-Type") != REQUEST_TYPE:
                    continue
                try:
                    request = json.loads(log.get("body", {}).get("stringValue", ""))
                except ValueError:
                    continue
                if not isinstance(request, dict):
                    continue
                prompts = _prompts(request)
                if not prompts:
                    continue
                t = _time_ns(_header(attrs, "Ce-Time"))
                if t is None:
                    t = int(log.get("timeUnixNano") or log.get("observedTimeUnixNano") or 0)
                rid = _header(attrs, "Ce-Id") or f"{t}"
                model = request.get("model") or _header(attrs, "Ce-Inferenceservicename") or ""
                for i, ids in enumerate(prompts):
                    yield TraceRecord(
                        timestamp_ns=t,
                        model=str(model),
                        input_ids=ids,
                        request_id=rid if len(prompts) == 1 else f"{rid}#{i}",
                    )


TRACE_FORMATS["custom"] = TraceFormat(
    read=read,
    expects=(
        "KServe inference request CloudEvents, which KServe's payload logger "
        "(mode: request) sends to the trace collector's KServe input"
    ),
)

if __name__ == "__main__":
    sys.exit(main())
