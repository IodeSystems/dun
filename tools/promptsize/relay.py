#!/usr/bin/env python3
"""A recording relay between a coding agent and a real model server.

Every OpenAI chat-completions or Anthropic messages request is forwarded to UPSTREAM (the key
from $KEY replaces whatever the agent sent) and logged to OUT/requests.jsonl
with the usage the server reported: one line per request, in order, so a
session's growth can be read off it (session.py). A streamed request gets
stream_options.include_usage so its last chunk carries the usage. GET
/models is forwarded too.

    KEY=… relay.py OUT PORT UPSTREAM
"""
import json
import os
import sys
import time
import urllib.error
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

OUT, PORT, UP = sys.argv[1], int(sys.argv[2]), sys.argv[3].rstrip("/")
KEY = os.environ.get("KEY", "")
os.makedirs(OUT, exist_ok=True)
LOG = os.path.join(OUT, "requests.jsonl")


def upstream_url(path):
    p = path.split("?")[0]
    if p.startswith("/v1/"):
        p = p[3:]
    return UP + "/v1" + p


class H(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, fmt, *a):
        pass

    def _forward_get(self):
        req = urllib.request.Request(upstream_url(self.path), headers={"authorization": "Bearer " + KEY})
        try:
            with urllib.request.urlopen(req, timeout=60) as r:
                body = r.read()
                self.send_response(r.status)
                self.send_header("content-type", r.headers.get("content-type", "application/json"))
                self.send_header("content-length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)
        except urllib.error.HTTPError as e:
            body = e.read()
            self.send_response(e.code)
            self.send_header("content-length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)

    do_GET = _forward_get

    def do_POST(self):
        raw = self.rfile.read(int(self.headers.get("content-length") or 0))
        body = json.loads(raw)
        stream = bool(body.get("stream"))
        anthropic = self.path.split("?")[0].endswith("/messages")
        if stream and not anthropic:
            body.setdefault("stream_options", {})["include_usage"] = True
        t0 = time.time()
        req = urllib.request.Request(upstream_url(self.path), json.dumps(body).encode(),
                                     {"content-type": "application/json", "authorization": "Bearer " + KEY})
        usage, status = None, 0
        try:
            r = urllib.request.urlopen(req, timeout=900)
            status = r.status
            self.send_response(r.status)
            self.send_header("content-type", r.headers.get("content-type", "application/json"))
            if stream:
                self.send_header("transfer-encoding", "chunked")
                self.end_headers()
                for line in r:
                    if line.startswith(b"data: ") and b'"usage"' in line:
                        try:
                            ev = json.loads(line[6:])
                            u = ev.get("usage") or (ev.get("message") or {}).get("usage")
                            if u:  # Anthropic: message_start has the input, message_delta the output
                                usage = dict(usage or {}, **{k: v for k, v in u.items() if v is not None})
                        except Exception:
                            pass
                    self.wfile.write(b"%x\r\n%s\r\n" % (len(line), line))
                    self.wfile.flush()
                self.wfile.write(b"0\r\n\r\n")
            else:
                data = r.read()
                self.send_header("content-length", str(len(data)))
                self.end_headers()
                self.wfile.write(data)
                try:
                    usage = json.loads(data).get("usage")
                except Exception:
                    pass
        except urllib.error.HTTPError as e:
            status = e.code
            data = e.read()
            self.send_response(e.code)
            self.send_header("content-length", str(len(data)))
            self.end_headers()
            self.wfile.write(data)
        if usage and "input_tokens" in usage:  # Anthropic: uncached input + cache reads + cache writes
            usage["prompt_tokens"] = (usage.get("input_tokens") or 0) + (usage.get("cache_read_input_tokens") or 0) + \
                (usage.get("cache_creation_input_tokens") or 0)
            usage["prompt_tokens_details"] = {"cached_tokens": usage.get("cache_read_input_tokens") or 0}
        with open(LOG, "a") as f:
            f.write(json.dumps({"t": t0, "secs": round(time.time() - t0, 2), "status": status, "path": self.path,
                                "messages": len(body.get("messages", [])), "tools": len(body.get("tools") or []),
                                "chars": len(raw), "usage": usage}) + "\n")


ThreadingHTTPServer(("127.0.0.1", PORT), H).serve_forever()
