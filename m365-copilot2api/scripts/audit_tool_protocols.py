#!/usr/bin/env python3
"""Opt-in live compatibility probes; credentials stay in an environment variable.

Only synthetic prompts are sent. Returned tools are inspected, never executed.
Use a loopback gateway unless --allow-remote is explicitly supplied.
"""
import argparse
import concurrent.futures
import json
import os
from pathlib import Path
import sys
import threading
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid

MODEL = "gpt-5.6-reasoning"
EFFORT = "max"
TOOL = "multi_agent_v1__spawn_agent"
PARAMETERS = {"type": "object", "properties": {
    "message": {"type": "string"}, "fork_context": {"type": "boolean"}},
    "required": ["message", "fork_context"], "additionalProperties": False}


def payload_for(protocol, stream, prompt, tools=False, serial=False, forbid=False):
    payload = {"model": MODEL, "stream": stream, "reasoning_effort": EFFORT}
    if protocol == "responses":
        payload.update(input=prompt, reasoning={"effort": EFFORT}, max_output_tokens=2048)
        if tools:
            payload["tools"] = [{"type": "function", "name": TOOL, "parameters": PARAMETERS}]
    else:
        payload.update(messages=[{"role": "user", "content": prompt}], max_tokens=2048)
        if protocol == "messages":
            # The gateway accepts its shared reasoning_effort extension here.
            payload["output_config"] = {"effort": EFFORT}
            if tools:
                payload["tools"] = [{"name": TOOL, "description": "Return an independent read-only task", "input_schema": PARAMETERS}]
        elif tools:
            payload["tools"] = [{"type": "function", "function": {"name": TOOL, "description": "Return an independent read-only task", "parameters": PARAMETERS}}]
    if tools:
        payload["tool_choice"] = {"type": "none" if forbid else "auto"} if protocol == "messages" else ("none" if forbid else "auto")
        if protocol != "messages":
            payload["parallel_tool_calls"] = not serial
        elif serial:
            payload["tool_choice"]["disable_parallel_tool_use"] = True
    return payload


def decode_reply(protocol, stream, raw):
    calls = []
    text = []
    state = None
    errors = []
    terminal_count = 0
    if not stream:
        body = json.loads(raw)
        if body.get("error"):
            errors.append("error_response")
        if protocol == "chat":
            choice = body.get("choices", [{}])[0]
            message = choice.get("message", {})
            text.append(message.get("content") or "")
            calls = message.get("tool_calls", [])
            state = choice.get("finish_reason")
        elif protocol == "messages":
            state = body.get("stop_reason")
            for block in body.get("content", []):
                if block.get("type") == "text": text.append(block.get("text", ""))
                if block.get("type") == "tool_use":
                    calls.append({"id": block.get("id"), "function": {"name": block.get("name"), "arguments": json.dumps(block.get("input", {}))}})
        else:
            state = body.get("status")
            for item in body.get("output", []):
                if item.get("type") == "function_call":
                    name = item.get("name", "")
                    if item.get("namespace"): name = item["namespace"] + "__" + name
                    calls.append({"id": item.get("call_id"), "function": {"name": name, "arguments": item.get("arguments", "")}})
                text.extend(c.get("text", "") for c in item.get("content", []) if c.get("type") == "output_text")
        return {"text": "".join(text), "calls": calls, "state": state, "errors": errors, "terminal_count": 1}
    indexed = {}
    terminal = None
    for line in raw.splitlines():
        if not line.startswith("data:"): continue
        data = line[5:].strip()
        if not data: continue
        if data == "[DONE]":
            if protocol == "chat": terminal_count += 1
            continue
        event = json.loads(data)
        kind = event.get("type", "")
        if event.get("error") or kind in ("error", "response.failed", "response.incomplete"):
            errors.append(kind or "error")
        if protocol == "chat":
            for choice in event.get("choices", []):
                delta = choice.get("delta", {})
                text.append(delta.get("content") or "")
                if choice.get("finish_reason"): state = choice["finish_reason"]
                for call in delta.get("tool_calls", []):
                    target = indexed.setdefault(call.get("index", 0), {"id": "", "function": {"name": "", "arguments": ""}})
                    if call.get("id"): target["id"] = call["id"]
                    for field in ("name", "arguments"):
                        target["function"][field] += call.get("function", {}).get(field, "")
        elif protocol == "messages":
            if kind == "content_block_start":
                block = event.get("content_block", {})
                if block.get("type") == "text": text.append(block.get("text", ""))
                if block.get("type") == "tool_use":
                    indexed[event["index"]] = {"id": block.get("id"), "function": {"name": block.get("name"), "arguments": ""}}
            elif kind == "content_block_delta":
                delta = event.get("delta", {})
                if delta.get("type") == "text_delta": text.append(delta.get("text", ""))
                if delta.get("type") == "input_json_delta":
                    indexed[event["index"]]["function"]["arguments"] += delta.get("partial_json", "")
            elif kind == "message_delta": state = event.get("delta", {}).get("stop_reason")
            elif kind == "message_stop": terminal_count += 1
        else:
            if kind == "response.output_text.delta": text.append(event.get("delta", ""))
            elif kind == "response.completed":
                terminal_count += 1
                terminal = decode_reply(protocol, False, json.dumps(event["response"]))
                state = terminal["state"]
    calls = [indexed[i] for i in sorted(indexed)]
    if protocol == "responses" and terminal:
        calls = terminal["calls"]
        # Completion snapshots must not be appended to the already emitted deltas.
        if "".join(text) != terminal["text"]: errors.append("delta_snapshot_mismatch")
    return {"text": "".join(text), "calls": calls, "state": state, "errors": errors, "terminal_count": terminal_count}


def run_case(base_url, key, protocol, stream, scenario, timeout, rate_limited=None):
    if rate_limited is not None and rate_limited.is_set():
        return {"case": f"{protocol}/{scenario}/stream={stream}", "model": MODEL, "effort": EFFORT, "passed": False, "skipped": "stopped after HTTP 429; honor Retry-After before a new run"}
    marker = "AUDIT_OK_" + uuid.uuid4().hex[:12]
    expected = ["A: literal ) and {; inspect only", "B: literal ( and }; inspect only", "C: 路径 C:\\audit\\notes.txt; inspect only"]
    if scenario in ("text", "none"):
        prompt = "Reply with exactly this token once, no tools or extra text: " + marker
    else:
        prompt = ("Emit exactly three independent calls to the declared tool in this single response, one per task. "
                  "Use fork_context=true. Copy these three message arguments verbatim in order; do not combine them, "
                  "do not run actual tasks or spawn additional agents, just return the tool calls: " + json.dumps(expected, ensure_ascii=False))
    payload = payload_for(protocol, stream, prompt, tools=scenario != "text", serial=scenario == "serial", forbid=scenario == "none")
    path = {"chat": "/v1/chat/completions", "messages": "/v1/messages", "responses": "/v1/responses"}[protocol]
    result = {"case": f"{protocol}/{scenario}/stream={stream}", "model": MODEL, "effort": EFFORT}
    started = time.monotonic()
    try:
        request = urllib.request.Request(base_url.rstrip("/") + path, data=json.dumps(payload).encode(), headers={"Authorization": "Bearer " + key, "Content-Type": "application/json"})
        opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
        with opener.open(request, timeout=timeout) as response:
            raw = response.read().decode("utf-8")
            result["http_status"] = response.status
        parsed = decode_reply(protocol, stream, raw)
        result.update(state=parsed["state"], tool_count=len(parsed["calls"]), terminal_count=parsed["terminal_count"], errors=parsed["errors"])
        passed = not parsed["errors"] and parsed["terminal_count"] == 1 and parsed["state"] not in (None, "failed", "incomplete", "length", "max_tokens")
        if scenario in ("text", "none"):
            passed = passed and parsed["text"].strip() == marker and not parsed["calls"]
            result["text_matches"] = parsed["text"].strip() == marker
        else:
            wanted = 1 if scenario == "serial" else 3
            calls = parsed["calls"]
            ids = [call.get("id") for call in calls]
            passed = passed and len(calls) == wanted and all(ids) and len(set(ids)) == len(ids)
            arguments = [json.loads(call["function"]["arguments"]) for call in calls]
            result["arguments_match"] = all(arg == {"fork_context": True, "message": expected[i]} and calls[i]["function"]["name"] == TOOL for i, arg in enumerate(arguments)) if len(calls) <= len(expected) else False
            passed = passed and result["arguments_match"]
        result["passed"] = bool(passed)
    except urllib.error.HTTPError as error:
        result.update(passed=False, http_status=error.code, error="HTTPError")
        if error.code == 429:
            result["retry_after"] = error.headers.get("Retry-After", "unspecified")
            if rate_limited is not None: rate_limited.set()
    except Exception as error:
        # Never log the request, credentials, or arbitrary upstream error bodies.
        result.update(passed=False, error=type(error).__name__)
    result["seconds"] = round(time.monotonic() - started, 3)
    return result


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--base-url", default="http://127.0.0.1:4141")
    parser.add_argument("--api-key-env", default="M365_TEST_API_KEY")
    parser.add_argument("--allow-remote", action="store_true")
    parser.add_argument("--suite", choices=("smoke", "tools", "all"), default="all")
    parser.add_argument("--workers", type=int, default=2)
    parser.add_argument("--timeout", type=float, default=120)
    parser.add_argument("--report", type=Path, required=True)
    args = parser.parse_args()
    host = urllib.parse.urlsplit(args.base_url).hostname
    if not args.allow_remote and host not in ("127.0.0.1", "localhost", "::1"):
        parser.error("non-loopback target requires --allow-remote")
    key = os.environ.get(args.api_key_env)
    if not key: parser.error("API key environment variable is empty")
    cases = []
    for protocol in ("chat", "messages", "responses"):
        for stream in (False, True):
            scenarios = ["text"] if args.suite == "smoke" else ["parallel", "none"]
            if args.suite == "all": scenarios.insert(0, "text")
            if args.suite != "smoke": scenarios.append("serial")
            cases.extend((protocol, stream, scenario) for scenario in scenarios)
    results = []
    rate_limited = threading.Event()
    with concurrent.futures.ThreadPoolExecutor(max_workers=max(1, min(args.workers, 4))) as executor:
        futures = [executor.submit(run_case, args.base_url, key, *case, args.timeout, rate_limited) for case in cases]
        for future in concurrent.futures.as_completed(futures):
            result = future.result()
            results.append(result)
            print(json.dumps(result, ensure_ascii=False), flush=True)
    report = {"model": MODEL, "effort": EFFORT, "scope": "Live synthetic text/tool wire probes; no returned tools are executed", "passed": sum(r["passed"] for r in results), "total": len(results), "results": sorted(results, key=lambda r: r["case"])}
    args.report.parent.mkdir(parents=True, exist_ok=True)
    args.report.write_text(json.dumps(report, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    return 0 if report["passed"] == report["total"] else 1


if __name__ == "__main__":
    sys.exit(main())
