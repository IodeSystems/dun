import http.server, json, sys
TOK = "tok-123"
RES = {
 "docs://runbook/deploy": ("Deploy runbook", "# Deploying to the zebra cluster\n\nDeploys to the zebra cluster require the --frostbite flag, because zebra nodes run the old kernel. Without it the rollout hangs at 40%.\n"),
 "docs://api/overview": ("API overview", "# Ticket API\n\nGET /tickets/{id} returns a ticket with fields id, title, severity.\n"),
 "skill://incident/SKILL.md": ("incident", "---\nname: incident-triage\ndescription: Triage a production incident ticket - use when the user asks to triage or investigate a ticket.\n---\n# Incident triage\n\n1. Look the ticket up with the lookup_ticket tool.\n2. Read checklist.md from this skill and apply it.\n3. Reply with severity, owner and next step.\n"),
 "skill://incident/checklist.md": ("checklist", "- If severity is sev1, page the on-call owner.\n- Always name the owner: team-orca.\n"),
}
class H(http.server.BaseHTTPRequestHandler):
    def log_message(self, *a): pass
    def do_DELETE(self): self.send_response(200); self.end_headers()
    def do_POST(self):
        if self.headers.get("Authorization") != "Bearer " + TOK:
            self.send_response(401); self.end_headers(); self.wfile.write(b"bad token"); return
        msg = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        if "id" not in msg:
            self.send_response(202); self.end_headers(); return
        m, p = msg["method"], msg.get("params") or {}
        print("REQ", m, json.dumps(p)[:120], file=sys.stderr, flush=True)
        if m == "initialize":
            r = {"protocolVersion": "2025-06-18", "capabilities": {"tools": {}, "resources": {}, "prompts": {}}, "serverInfo": {"name": "fakecorp", "version": "1"}}
        elif m == "tools/list":
            r = {"tools": [{"name": "lookup_ticket", "description": "Look up a ticket by id", "inputSchema": {"type": "object", "properties": {"id": {"type": "string"}}, "required": ["id"]}}]}
        elif m == "tools/call":
            r = {"content": [{"type": "text", "text": json.dumps({"id": p["arguments"].get("id"), "title": "Checkout returns 500", "severity": "sev1"})}]}
        elif m == "resources/list":
            r = {"resources": [{"uri": u, "name": n, "mimeType": "text/markdown"} for u, (n, _) in RES.items()]}
        elif m == "resources/read":
            r = {"contents": [{"uri": p["uri"], "mimeType": "text/markdown", "text": RES[p["uri"]][1]}]}
        elif m == "prompts/list":
            r = {"prompts": [{"name": "triage", "description": "Triage a ticket", "arguments": [{"name": "ticket", "required": True}]}]}
        elif m == "prompts/get":
            r = {"messages": [{"role": "user", "content": {"type": "text", "text": "Triage ticket " + p["arguments"]["ticket"] + " and tell me severity, owner and next step."}}]}
        else:
            r = None
        body = json.dumps({"jsonrpc": "2.0", "id": msg["id"], "result": r}).encode()
        sse = m == "tools/list"
        self.send_response(200)
        if sse:
            self.send_header("Content-Type", "text/event-stream"); self.end_headers()
            self.wfile.write(b"data: " + body + b"\n\n")
        else:
            if m == "initialize": self.send_header("Mcp-Session-Id", "sess-1")
            self.send_header("Content-Type", "application/json"); self.send_header("Content-Length", str(len(body))); self.end_headers()
            self.wfile.write(body)
http.server.ThreadingHTTPServer(("127.0.0.1", 18790), H).serve_forever()
