#!/usr/bin/env python3
"""A local, instant stand-in for the model endpoint so provider latency stays
out of the lifecycle numbers: metadata routes answer at once, and a chat turn
streams a short fixed reply (no tool calls), which is enough for Wakil to save
a transcript that /resume can later load."""
import json, sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

class H(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    def log_message(self, *a): pass
    def _json(self, code, obj):
        b = json.dumps(obj).encode()
        self.send_response(code); self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(b))); self.end_headers(); self.wfile.write(b)
    def do_GET(self):
        p = self.path.split("?")[0]
        if p == "/v1/models":
            self._json(200, {"object": "list", "data": [{"id": "fake-model", "object": "model"}]})
        elif p == "/props":
            self._json(200, {"default_generation_settings": {"n_ctx": 32768}})
        else:
            self._json(404, {"error": "not found"})
    def do_POST(self):
        n = int(self.headers.get("Content-Length") or 0)
        if n: self.rfile.read(n)
        if self.path.split("?")[0] != "/v1/chat/completions":
            return self._json(404, {"error": "not found"})
        chunks = [
            {"choices": [{"index": 0, "delta": {"role": "assistant", "content": "Understood. "}}]},
            {"choices": [{"index": 0, "delta": {"content": "Nothing else to do."}}]},
            {"choices": [{"index": 0, "delta": {}, "finish_reason": "stop"}],
             "usage": {"prompt_tokens": 12, "completion_tokens": 6, "total_tokens": 18}},
        ]
        body = "".join("data: " + json.dumps(c) + "\n\n" for c in chunks) + "data: [DONE]\n\n"
        b = body.encode()
        self.send_response(200); self.send_header("Content-Type", "text/event-stream")
        self.send_header("Cache-Control", "no-cache"); self.send_header("Content-Length", str(len(b)))
        self.end_headers(); self.wfile.write(b)

ThreadingHTTPServer(("127.0.0.1", int(sys.argv[1])), H).serve_forever()
