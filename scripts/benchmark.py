#!/usr/bin/env python3
"""kaal latency benchmark — p50..p99 for an OpenAI-compatible endpoint.

Measures, per request: TTFT (first streamed content byte), total wall time,
and approximate throughput. Reports percentiles for the whole run.

Usage:
  python3 scripts/benchmark.py [--base-url URL] [--model ID] [--requests N]
        [--max-tokens N] [--prompt TEXT] [--api-key KEY] [--json OUT]

Defaults ride kaal's own setup: opencode zen's keyless free tier
(--base-url https://opencode.ai/zen/v1 --model hy3-free, no auth).
Point --base-url at https://api.commandcode.ai/provider/v1 (with --api-key)
or https://opencode.ai/zen/go/v1 to compare routes.

NOTE: this measures the raw wire — gateway dial + prompt processing +
generation. The agent loop adds tool-execution time on top.
"""
import argparse
import json
import sys
import time
import urllib.request

def percentile(sorted_vals, p):
    if not sorted_vals:
        return 0.0
    import math
    idx = max(0, math.ceil(p / 100 * len(sorted_vals)) - 1)
    return sorted_vals[min(idx, len(sorted_vals) - 1)]

def one_request(base_url, model, api_key, prompt, max_tokens, timeout):
    """Returns (ttft, total, chars). ttft=None when nothing streamed."""
    body = json.dumps({
        "model": model,
        "messages": [{"role": "user", "content": prompt}],
        "max_tokens": max_tokens,
        "stream": True,
    }).encode()
    req = urllib.request.Request(
        base_url.rstrip("/") + "/chat/completions", data=body,
        headers={"Content-Type": "application/json",
                 "User-Agent": "python-requests/2.31.0",
                 **({"Authorization": "Bearer " + api_key} if api_key else {})})
    t0 = time.perf_counter()
    resp = urllib.request.urlopen(req, timeout=timeout)
    ttft, chars = None, 0
    with resp:
        for raw in resp:
            line = raw.decode("utf-8", "replace").strip()
            if not line.startswith("data:"):
                continue
            data = line[5:].strip()
            if data == "[DONE]":
                break
            try:
                chunk = json.loads(data)
            except json.JSONDecodeError:
                continue
            choices = chunk.get("choices") or [{}]
            delta = choices[0].get("delta") or {}
            text = delta.get("content") or delta.get("reasoning_content")
            if text:
                if ttft is None:
                    ttft = time.perf_counter() - t0
                chars += len(text)
    return ttft, time.perf_counter() - t0, chars

def report(name, vals):
    vals = sorted(vals)
    n = len(vals)
    mean = sum(vals) / n
    print(f"{name:>10}  p50={percentile(vals,50):7.3f}s  p75={percentile(vals,75):7.3f}s  "
          f"p90={percentile(vals,90):7.3f}s  p95={percentile(vals,95):7.3f}s  "
          f"p99={percentile(vals,99):7.3f}s  mean={mean:7.3f}s  "
          f"min={vals[0]:7.3f}s  max={vals[-1]:7.3f}s")

def main():
    ap = argparse.ArgumentParser(description="kaal latency benchmark")
    ap.add_argument("--base-url", default="https://opencode.ai/zen/v1")
    ap.add_argument("--model", default="hy3-free")
    ap.add_argument("--api-key", default=os_environ_key())
    ap.add_argument("--requests", type=int, default=20)
    ap.add_argument("--max-tokens", type=int, default=120)
    ap.add_argument("--prompt", default="Count from 1 to 20, digits separated by spaces.")
    ap.add_argument("--timeout", type=int, default=90)
    ap.add_argument("--json", dest="json_out", default="")
    args = ap.parse_args()

    print(f"kaal bench · {args.base_url} · {args.model} · {args.requests} requests "
          f"(max_tokens={args.max_tokens})")
    ttfts, totals, fails = [], [], 0
    for i in range(1, args.requests + 1):
        try:
            ttft, total, chars = one_request(
                args.base_url, args.model, args.api_key,
                args.prompt, args.max_tokens, args.timeout)
            ttfts.append(ttft if ttft is not None else total)
            totals.append(total)
            print(f"  [{i:>3}/{args.requests}] ttft={ttft if ttft else -1:6.2f}s "
                  f"total={total:6.2f}s chars={chars}")
        except Exception as exc:  # noqa: BLE001 — report and keep going
            fails += 1
            print(f"  [{i:>3}/{args.requests}] FAILED: {exc}")
        time.sleep(0.2)

    print(f"\n=== results ({len(totals)} ok, {fails} failed)")
    if ttfts:
        report("ttft", ttfts)
        report("total", totals)
    if args.json_out:
        with open(args.json_out, "w") as fh:
            json.dump({"ttft": ttfts, "total": totals, "failed": fails}, fh)
        print(f"wrote {args.json_out}")
    return 1 if fails and not totals else 0

def os_environ_key():
    for name in ("OPENCODE_API_KEY", "CMD_API_KEY"):
        if os_environ_get(name):
            return os_environ_get(name)
    return ""

def os_environ_get(name):
    import os
    return os.environ.get(name, "")

if __name__ == "__main__":
    sys.exit(main())
