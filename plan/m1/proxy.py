"""Logging pass-through proxy: every request body and its full response go to <dir>/NNN-{req,resp}."""
import http.server, socketserver, sys, os, urllib.request, itertools, threading, time
UP, DIR, PORT = sys.argv[1].rstrip("/"), sys.argv[2], int(sys.argv[3])
n = itertools.count(1); lock = threading.Lock()
class H(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    def _go(self):
        body = self.rfile.read(int(self.headers.get("Content-Length") or 0))
        with lock: i = next(n)
        open(f"{DIR}/{i:03d}-req.json", "wb").write(body)
        hdr = {k: v for k, v in self.headers.items() if k.lower() not in ("host", "content-length", "accept-encoding", "connection")}
        req = urllib.request.Request(UP + self.path, data=body if self.command == "POST" else None, headers=hdr, method=self.command)
        t0 = time.time()
        try: r = urllib.request.urlopen(req, timeout=600)
        except urllib.error.HTTPError as e: r = e
        self.send_response(r.status)
        for k, v in r.headers.items():
            if k.lower() not in ("transfer-encoding", "connection", "content-length"): self.send_header(k, v)
        self.send_header("Transfer-Encoding", "chunked"); self.end_headers()
        out = open(f"{DIR}/{i:03d}-resp.txt", "wb")
        while True:
            c = r.read1(65536) if hasattr(r, "read1") else r.read(65536)
            if not c: break
            out.write(c); self.wfile.write(b"%x\r\n%s\r\n" % (len(c), c)); self.wfile.flush()
        self.wfile.write(b"0\r\n\r\n"); out.close()
        open(f"{DIR}/{i:03d}-meta.txt", "w").write(f"{self.command} {self.path} {r.status} t0={t0:.3f} dt={time.time()-t0:.2f}\n")
    do_POST = do_GET = _go
    def log_message(self, *a): pass
class S(socketserver.ThreadingMixIn, http.server.HTTPServer): daemon_threads = True
S(("127.0.0.1", PORT), H).serve_forever()
