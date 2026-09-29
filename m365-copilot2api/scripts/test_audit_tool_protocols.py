import io
import json
import threading
import unittest
import urllib.error
from unittest.mock import patch

from audit_tool_protocols import MODEL, EFFORT, decode_reply, payload_for, run_case


class AuditRunnerTests(unittest.TestCase):
    def test_messages_requests_max_and_serial_control(self):
        body = payload_for("messages", True, "synthetic", tools=True, serial=True)
        self.assertEqual(body["model"], MODEL)
        self.assertEqual(body["output_config"]["effort"], EFFORT)
        self.assertTrue(body["tool_choice"]["disable_parallel_tool_use"])

    def test_rate_limit_stops_queued_cases(self):
        stopped = threading.Event()
        error = urllib.error.HTTPError("http://127.0.0.1/", 429, "limited", {"Retry-After": "60"}, io.BytesIO())
        with patch("audit_tool_protocols.urllib.request.build_opener") as factory:
            factory.return_value.open.side_effect = error
            first = run_case("http://127.0.0.1", "synthetic", "chat", False, "text", 1, stopped)
            second = run_case("http://127.0.0.1", "synthetic", "responses", False, "text", 1, stopped)
        self.assertFalse(first["passed"])
        self.assertEqual(first["retry_after"], "60")
        self.assertTrue(stopped.is_set())
        self.assertIn("skipped", second)
        self.assertEqual(factory.call_count, 1)

    def test_responses_snapshot_is_not_appended_twice(self):
        terminal = {"status": "completed", "output": [{"type": "message", "content": [{"type": "output_text", "text": "OK"}]}]}
        events = [{"type": "response.output_text.delta", "delta": "OK"}, {"type": "response.completed", "response": terminal}]
        reply = decode_reply("responses", True, "\n\n".join("data: " + json.dumps(e) for e in events))
        self.assertEqual(reply["text"], "OK")
        self.assertEqual(reply["terminal_count"], 1)
        self.assertEqual(reply["errors"], [])

    def test_messages_duplicate_start_text_is_detectable(self):
        events = [
            {"type": "content_block_start", "index": 0, "content_block": {"type": "text", "text": "OK"}},
            {"type": "content_block_delta", "index": 0, "delta": {"type": "text_delta", "text": "OK"}},
            {"type": "message_delta", "delta": {"stop_reason": "end_turn"}},
            {"type": "message_stop"},
        ]
        reply = decode_reply("messages", True, "\n\n".join("data: " + json.dumps(e) for e in events))
        self.assertEqual(reply["text"], "OKOK")
        self.assertNotEqual(reply["text"], "OK")

    def test_failed_response_cannot_count_as_completed(self):
        reply = decode_reply("responses", True, 'data: {"type":"response.failed","error":{"message":"synthetic"}}\n\n')
        self.assertIn("response.failed", reply["errors"])
        self.assertEqual(reply["terminal_count"], 0)


if __name__ == "__main__":
    unittest.main()
