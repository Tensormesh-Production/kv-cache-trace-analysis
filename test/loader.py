"""Send synthetic request traces to a trace collector, for tests.

Builds chat requests with the reuse patterns the analyzer measures (shared
system prompts, growing multi-turn chats, documents reused in different
orders, unique requests) and sends them over OTLP/HTTP JSON, 8 records per
export:

  dynamo   Dynamo frontend request_payload log records
  lmcache  LMCache mp.request.trace spans (token ids from a toy word tokenizer)
  kserve   KServe payload logger CloudEvents, one HTTP POST per event, to the
           collector's KServe input; with --file, the log records that the
           collector writes. Every 10th request also has a response event, and
           3 posts are not KServe events: the collector and the reader drop both.

Standard library only; no real data.

Usage:
  python3 loader.py --endpoint http://trace-collector:4318 --format both
  python3 loader.py --endpoint http://trace-collector:8088/events --format kserve
  python3 loader.py --file trace.jsonl --format both     (write instead of send)
"""

import argparse
import json
import random
import sys
import time
import urllib.request
from datetime import datetime, timezone

WORDS = (
    "cache token prefix model request latency memory server engine batch "
    "router chunk vector index query answer context window policy tenant "
    "cluster node volume report metric trace sample result value zebrafinch"
).split()
ROLE_IDS = {"system": 1, "user": 2, "assistant": 3}


def text(rng, n_words):
    return " ".join(rng.choice(WORDS) for _ in range(n_words))


def build_requests(n, seed):
    """Returns (time_ms, model, request_id, chat request) tuples."""
    rng = random.Random(seed)
    systems = [text(rng, 700), text(rng, 400)]
    docs = [text(rng, rng.randint(200, 600)) for _ in range(30)]
    chats = {}
    t = 1_793_000_000_000
    out = []
    for i in range(n):
        t += rng.randint(50, 3000)
        kind = rng.random()
        model = "model-a" if rng.random() < 0.8 else "model-b"
        if kind < 0.4:  # multi-turn chat that grows
            cid = rng.randrange(25)
            hist = chats.setdefault(
                cid, [{"role": "system", "content": systems[cid % 2]}]
            )
            hist = hist + [{"role": "user", "content": text(rng, rng.randint(20, 120))}]
            messages = hist
            chats[cid] = hist + [
                {"role": "assistant", "content": text(rng, rng.randint(20, 80))}
            ]
        elif kind < 0.8:  # system + 3 documents in random order + question
            picked = rng.sample(range(len(docs)), 3)
            context = "\n\n".join(docs[d] for d in picked)
            messages = [
                {"role": "system", "content": systems[0]},
                {"role": "user", "content": context + "\n\nQuestion: " + text(rng, 25)},
            ]
        else:  # unique
            messages = [{"role": "user", "content": text(rng, rng.randint(150, 1500))}]
        out.append((t, model, f"req-{i}", {"messages": messages, "model": model}))
    return out


def token_ids(request):
    """Toy tokenizer: one id per role marker and per word."""
    ids = []
    for message in request["messages"]:
        ids.append(ROLE_IDS[message["role"]])
        ids.extend(1000 + WORDS.index(w) for w in message["content"].split() if w in WORDS)
    return ids


def _s(key, value):
    return {"key": key, "value": {"stringValue": value}}


def dynamo_record(t_ms, model, rid, request):
    rec = {
        "schema": "dynamo.request.trace.v1",
        "event_type": "request_payload",
        "event_time_unix_ms": t_ms,
        "event_source": "dynamo",
        "payload": {
            "request_id": rid,
            "endpoint": "openai.chat_completion",
            "model": model,
            "request": request,
            "payload_complete": True,
        },
    }
    return {
        "timeUnixNano": str(t_ms * 1_000_000),
        "severityNumber": 9,
        "severityText": "INFO",
        "body": {"stringValue": "request_payload"},
        "attributes": [
            _s("schema", "dynamo.request.trace.v1"),
            _s("event_type", "request_payload"),
            _s("rid", rid),
            _s("model", model),
            _s("endpoint", "openai.chat_completion"),
            {"key": "payload_complete", "value": {"boolValue": True}},
            _s("payload", json.dumps(rec, separators=(",", ":"))),
        ],
    }


def lmcache_span(index, t_ms, model, rid, request):
    ids = token_ids(request)
    return {
        "traceId": f"{index + 1:032x}",
        "spanId": f"{index + 1:016x}",
        "name": "mp.request.trace",
        "kind": 1,
        "startTimeUnixNano": str(t_ms * 1_000_000),
        "endTimeUnixNano": str(t_ms * 1_000_000 + 1000),
        "attributes": [
            _s("session_id", rid),
            _s("lmcache.model", model),
            {
                "key": "lmcache.input_ids",
                "value": {"arrayValue": {"values": [{"intValue": str(t)} for t in ids]}},
            },
        ],
    }


def dynamo_batch(records):
    return "/v1/logs", {
        "resourceLogs": [
            {
                "resource": {"attributes": [_s("service.name", "dynamo")]},
                "scopeLogs": [
                    {"scope": {"name": "dynamo.request_trace"}, "logRecords": records}
                ],
            }
        ]
    }


def lmcache_batch(spans):
    return "/v1/traces", {
        "resourceSpans": [
            {
                "resource": {"attributes": [_s("service.name", "lmcache")]},
                "scopeSpans": [{"scope": {"name": "lmcache_mp.server"}, "spans": spans}],
            }
        ]
    }


KSERVE_REQUEST = "org.kubeflow.serving.inference.request"
KSERVE_RESPONSE = "org.kubeflow.serving.inference.response"


def kserve_events(reqs):
    """(headers, body) of the posts that KServe's agent would send, plus posts
    that are not KServe requests."""
    out = []
    for i, (t_ms, model, rid, request) in enumerate(reqs):
        sec, ms = divmod(t_ms, 1000)
        stamp = datetime.fromtimestamp(sec, tz=timezone.utc).strftime("%Y-%m-%dT%H:%M:%S")
        headers = {
            "Content-Type": "application/json",
            "Ce-Specversion": "1.0",
            "Ce-Id": rid,
            "Ce-Type": KSERVE_REQUEST,
            "Ce-Source": "http://localhost:9081/",
            "Ce-Time": f"{stamp}.{ms:03d}000001Z",
            "Ce-Inferenceservicename": model,
            "Ce-Namespace": "llm",
            "Ce-Component": "predictor",
            "Ce-Endpoint": "default",
        }
        out.append((headers, json.dumps(request)))
        if i % 10 == 0:
            answer = {"choices": [{"message": {"role": "assistant", "content": "kserve-answer " + text(random.Random(i), 30)}}]}
            out.append((dict(headers, **{"Ce-Type": KSERVE_RESPONSE}), json.dumps(answer)))
    for k in range(3):
        out.append(({"Content-Type": "application/json"}, json.dumps({"not": "kserve", "k": k})))
    return out


def kserve_batch(events):
    """The OTLP JSON line that the collector writes for these posts."""
    records = []
    for headers, body in events:
        attrs = [
            {"key": "header." + k, "value": {"arrayValue": {"values": [{"stringValue": v}]}}}
            for k, v in headers.items()
            if k.startswith("Ce-")
        ]
        records.append({"observedTimeUnixNano": "1", "body": {"stringValue": body}, "attributes": attrs})
    return {
        "resourceLogs": [
            {
                "resource": {},
                "scopeLogs": [
                    {
                        "scope": {
                            "name": "otlp/webhook_event",
                            "version": "0.159.0",
                            "attributes": [_s("source", "webhook_event/kserve"), _s("receiver", "webhook_event")],
                        },
                        "logRecords": records,
                    }
                ],
            }
        ]
    }


def send(endpoint, path, body, headers=None):
    req = urllib.request.Request(
        endpoint.rstrip("/") + path,
        data=body,
        headers=headers or {"Content-Type": "application/json"},
        method="POST",
    )
    for attempt in range(10):
        try:
            with urllib.request.urlopen(req, timeout=30) as resp:
                resp.read()
                return
        except Exception as exc:  # retry while the collector starts
            if attempt == 9:
                raise
            print(f"send failed ({exc}), retrying", file=sys.stderr)
            time.sleep(3)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--endpoint", default="http://trace-collector:4318")
    ap.add_argument("--file", help="write OTLP JSON lines here instead of sending")
    ap.add_argument("--format", choices=["dynamo", "lmcache", "kserve", "both"], default="both")
    ap.add_argument("--requests", type=int, default=300)
    ap.add_argument("--seed", type=int, default=7)
    args = ap.parse_args()

    reqs = build_requests(args.requests, args.seed)
    formats = ["dynamo", "lmcache"] if args.format == "both" else [args.format]
    out = open(args.file, "w", encoding="utf-8") if args.file else None
    sent = 0
    if formats == ["kserve"]:
        events = kserve_events(reqs)
        for k in range(0, len(events), 8):
            chunk = events[k : k + 8]
            if out:
                body = json.dumps(kserve_batch(chunk), separators=(",", ":"))
                sent += len(body)
                out.write(body + "\n")
                continue
            for headers, body in chunk:
                sent += len(body)
                send(args.endpoint, "", body.encode(), headers)
        formats = []
        print(f"{len(events)} KServe posts: {len(reqs)} requests, {sent / 1e6:.1f} MB", file=sys.stderr)
    for fmt in formats:
        for k in range(0, len(reqs), 8):
            chunk = reqs[k : k + 8]
            if fmt == "dynamo":
                path, batch = dynamo_batch([dynamo_record(*r) for r in chunk])
            else:
                path, batch = lmcache_batch(
                    [lmcache_span(k + j, *r) for j, r in enumerate(chunk)]
                )
            body = json.dumps(batch, separators=(",", ":"))
            sent += len(body)
            if out:
                out.write(body + "\n")
            else:
                send(args.endpoint, path, body.encode())
    if out:
        out.close()
    if formats:
        print(f"{len(reqs)} requests per format ({', '.join(formats)}), {sent / 1e6:.1f} MB", file=sys.stderr)


if __name__ == "__main__":
    main()
