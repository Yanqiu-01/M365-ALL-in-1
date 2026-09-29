#!/usr/bin/env python3
"""Live Edit recovery in an isolated temporary file, never the user's workspace."""
import argparse
import json
import os
from pathlib import Path
import sys
import tempfile
import time
import urllib.parse
import urllib.error
import urllib.request
import uuid

from audit_tool_protocols import MODEL, EFFORT, decode_reply

READ_SCHEMA = {"type": "object", "properties": {"file_path": {"type": "string", "enum": ["audit.txt"]}}, "required": ["file_path"]}
EDIT_SCHEMA = {"type": "object", "properties": {"file_path": {"type": "string", "enum": ["audit.txt"]}, "old_string": {"type": "string"}, "new_string": {"type": "string"}}, "required": ["file_path", "old_string", "new_string"]}


def convert_history(protocol, history):
    if protocol == "chat": return history
    result = []
    for message in history:
        if message.get("tool_calls"):
            blocks = []
            for call in message["tool_calls"]:
                fn = call["function"]
                if protocol == "responses":
                    result.append({"type": "function_call", "call_id": call["id"], "name": fn["name"], "arguments": fn["arguments"]})
                else:
                    blocks.append({"type": "tool_use", "id": call["id"], "name": fn["name"], "input": json.loads(fn["arguments"])})
            if protocol == "messages": result.append({"role": "assistant", "content": blocks})
        elif message["role"] == "tool":
            if protocol == "responses": result.append({"type": "function_call_output", "call_id": message["tool_call_id"], "output": message["content"]})
            else: result.append({"role": "user", "content": [{"type": "tool_result", "tool_use_id": message["tool_call_id"], "content": message["content"]}]})
        else: result.append(message)
    return result


def run(protocol, base, key, timeout):
    start = time.monotonic()
    events, errors = [], []
    rate_limit = {}
    seed = "seed-" + uuid.uuid4().hex
    history = [{"role": "user", "content": "In audit.txt change the line containing old to new, preserving its tab indentation and all other lines. Recover from the failed Edit below by reading current content first. Verify the file after the successful edit. Use only the declared Read and Edit tools."},
               {"role": "assistant", "content": None, "tool_calls": [{"id": seed, "type": "function", "function": {"name": "Edit", "arguments": json.dumps({"file_path": "audit.txt", "old_string": "\t\told", "new_string": "\tnew"})}}]},
               {"role": "tool", "tool_call_id": seed, "content": "<tool_use_error>String to replace not found in file.</tool_use_error>"}]
    with tempfile.TemporaryDirectory(prefix="m365-edit-audit-") as folder:
        target = Path(folder) / "audit.txt"
        target.write_text("before\n\told\nafter\n", encoding="utf-8", newline="")
        assert "\t\told" not in target.read_text(encoding="utf-8")
        fresh_read = False
        edited = False
        verified = False
        for round_no in range(1, 7):
            payload = {"model": MODEL, "reasoning_effort": EFFORT, "stream": False, "max_tokens": 2048}
            defs = [("Read", READ_SCHEMA), ("Edit", EDIT_SCHEMA)]
            if protocol == "chat":
                path = "/v1/chat/completions"; payload["messages"] = history
                payload["tools"] = [{"type": "function", "function": {"name": n, "parameters": s}} for n, s in defs]
            elif protocol == "messages":
                path = "/v1/messages"; payload["messages"] = convert_history(protocol, history)
                payload["output_config"] = {"effort": EFFORT}
                payload["tools"] = [{"name": n, "input_schema": s} for n, s in defs]
            else:
                path = "/v1/responses"; payload.pop("max_tokens"); payload["max_output_tokens"] = 2048
                payload["reasoning"] = {"effort": EFFORT}; payload["input"] = convert_history(protocol, history)
                payload["tools"] = [{"type": "function", "name": n, "parameters": s} for n, s in defs]
            request = urllib.request.Request(base.rstrip("/") + path, data=json.dumps(payload).encode(), headers={"Authorization": "Bearer " + key, "Content-Type": "application/json"})
            opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
            try:
                with opener.open(request, timeout=timeout) as response:
                    reply = decode_reply(protocol, False, response.read().decode("utf-8"))
            except urllib.error.HTTPError as error:
                errors.append("HTTPError")
                if error.code == 429:
                    rate_limit = {"http_status": 429, "retry_after": error.headers.get("Retry-After", "unspecified")}
                break
            except Exception as error:
                errors.append(type(error).__name__); break
            if reply["errors"]: errors.extend(reply["errors"]); break
            if not reply["calls"]:
                if not verified: errors.append("stopped_before_read_edit_verify")
                break
            calls = reply["calls"]
            history.append({"role": "assistant", "content": None, "tool_calls": [{**c, "type": "function"} for c in calls]})
            # A Read selected alongside Edit is not evidence that the read has
            # completed before that Edit was issued.
            batch_had_fresh_read = fresh_read
            for call in calls:
                name = call["function"]["name"]
                arguments = json.loads(call["function"]["arguments"])
                events.append({"round": round_no, "tool": name})
                if arguments.get("file_path") != "audit.txt" or name not in ("Read", "Edit"):
                    errors.append("out_of_scope_tool"); result = "Error: forbidden audit path or tool"
                elif name == "Read":
                    content = target.read_text(encoding="utf-8")
                    result = "\n".join(f"{i}\t{line}" for i, line in enumerate(content.splitlines(), 1))
                    fresh_read = True
                    if edited and content == "before\n\tnew\nafter\n": verified = True
                elif not batch_had_fresh_read:
                    errors.append("edit_before_completed_fresh_read"); result = "<tool_use_error>File has been modified since read.</tool_use_error>"
                else:
                    content = target.read_text(encoding="utf-8")
                    old, new = arguments.get("old_string", ""), arguments.get("new_string", "")
                    if not old or old == new or content.count(old) != 1:
                        result = "<tool_use_error>String to replace not found in file.</tool_use_error>"; fresh_read = False
                    else:
                        target.write_text(content.replace(old, new, 1), encoding="utf-8", newline="")
                        result = "File updated successfully."; edited = True
                history.append({"role": "tool", "tool_call_id": call["id"], "content": result})
            if verified: break
        final_matches = target.read_text(encoding="utf-8") == "before\n\tnew\nafter\n"
    return {**rate_limit, "protocol": protocol, "model": MODEL, "effort": EFFORT, "passed": not errors and verified and final_matches, "verified_by_read": verified, "final_file_matches": final_matches, "errors": errors, "events": events, "seconds": round(time.monotonic() - start, 3)}


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("--base-url", default="http://127.0.0.1:4141")
    p.add_argument("--api-key-env", default="M365_TEST_API_KEY")
    p.add_argument("--timeout", type=float, default=180)
    p.add_argument("--report", required=True, type=Path)
    args = p.parse_args()
    if urllib.parse.urlsplit(args.base_url).hostname not in ("localhost", "127.0.0.1", "::1"): p.error("only loopback targets are allowed")
    key = os.environ.get(args.api_key_env)
    if not key: p.error("API key environment variable is empty")
    results = []
    for protocol in ("chat", "messages", "responses"):
        result = run(protocol, args.base_url, key, args.timeout); results.append(result)
        print(json.dumps(result), flush=True)
        if result.get("http_status") == 429:
            print("Remaining protocols skipped after HTTP 429; honor Retry-After before a new run.", flush=True)
            break
    args.report.parent.mkdir(parents=True, exist_ok=True)
    args.report.write_text(json.dumps({"model": MODEL, "effort": EFFORT, "results": results}, indent=2) + "\n", encoding="utf-8")
    return 0 if all(r["passed"] for r in results) else 1


if __name__ == "__main__": sys.exit(main())
