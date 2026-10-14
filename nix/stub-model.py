# VM test stand-in for a model endpoint and an MCP server (stdlib only).
#   GET  /v1/models            -> one model
#   POST /v1/chat/completions  -> asks for the echo tool once, then answers
#                                 with the tool's result
#   POST /mcp                  -> minimal streamable-HTTP MCP: initialize,
#                                 tools/list (echo + 3 GitHub tools), tools/call
# Every MCP tools/call is appended to /tmp/mcp-calls so the test can see it,
# with a hash of the Authorization header it came with (never the header).
import hashlib
import json
import socket
import sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

TOOL = {"name": "echo", "description": "Echo the text back",
        "inputSchema": {"type": "object", "properties": {"text": {"type": "string"}}, "required": ["text"]}}
# The demo's GitHub tools: get_job_logs returns the file named in argv[1].
GH = [{"name": n, "description": d, "inputSchema": {"type": "object", "properties": {}}} for n, d in (
    ("get_workflow_run", "Get a workflow run"), ("list_workflow_jobs", "List a run's jobs"),
    ("get_job_logs", "Get a job's logs"))]
DIAGNOSIS = ("1. The go test job failed in package ./cart.\n2. TestTotalRounding expects 1999 but got 2000.\n"
             "3. The cause: the new rounding in cart/total.go rounds up instead of to nearest.\n"
             "4. The failing step is: go test ./...\n5. Fix: revert the rounding change or use math.Round.")
DIGEST = ("- CI broke on main: TestTotalRounding fails after the cart rounding change; a fix is pending review.\n"
          "- The agent's diagnosis was approved and ran once; no other agent runs.\n"
          "- The shop site was down once overnight (Uptime Kuma); one notification went out.\n"
          "- No approvals are waiting.\n"
          "- Next: merge the rounding fix, then watch the next CI run.")


def job_logs():
    try:
        return open(sys.argv[1]).read()
    except (IndexError, OSError):
        return "go test ./... FAIL: TestTotalRounding"


class H(BaseHTTPRequestHandler):
    def reply(self, code, body, extra=None):
        b = json.dumps(body).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        for k, v in (extra or {}).items():
            self.send_header(k, v)
        self.send_header("Content-Length", str(len(b)))
        self.end_headers()
        self.wfile.write(b)

    def do_GET(self):
        if self.path == "/v1/models":
            return self.reply(200, {"data": [{"id": "stub-model"}, {"id": "llama3.2"}]})
        self.send_response(405)  # no SSE stream on GET /mcp
        self.send_header("Content-Length", "0")
        self.end_headers()

    def do_DELETE(self):
        self.send_response(200)
        self.send_header("Content-Length", "0")
        self.end_headers()

    def do_POST(self):
        req = json.loads(self.rfile.read(int(self.headers.get("Content-Length", 0))) or b"{}")
        if self.path == "/v1/chat/completions":
            msgs = req.get("messages", [])
            tool_msgs = [m for m in msgs if m.get("role") == "tool"]
            text = " ".join(str(m.get("content")) for m in msgs)
            names = [t["function"]["name"] for t in req.get("tools") or []]
            if tool_msgs:
                out = DIAGNOSIS if "Workflow run" in text else "final: " + str(tool_msgs[-1].get("content"))
                msg = {"role": "assistant", "content": out}
            elif names:
                msg = {"role": "assistant", "content": None, "tool_calls": [{
                    "id": "call1", "type": "function",
                    "function": {"name": "get_job_logs" if "get_job_logs" in names else names[0], "arguments": json.dumps({"text": "hi from the model"})}}]}
            else:
                msg = {"role": "assistant", "content": DIGEST if "digest" in text else "plain answer"}
            return self.reply(200, {"id": "x", "object": "chat.completion", "model": req.get("model"),
                                    "choices": [{"index": 0, "message": msg, "finish_reason": "stop"}],
                                    "usage": {"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2}})
        if self.path == "/mcp":
            if "id" not in req:  # a notification
                self.send_response(202)
                self.send_header("Content-Length", "0")
                return self.end_headers()
            method, rid = req.get("method"), req["id"]
            if method == "initialize":
                ver = req.get("params", {}).get("protocolVersion", "2025-06-18")
                return self.reply(200, {"jsonrpc": "2.0", "id": rid, "result": {
                    "protocolVersion": ver, "capabilities": {"tools": {}},
                    "serverInfo": {"name": "stub", "version": "1"}}}, {"Mcp-Session-Id": "s1"})
            if method == "tools/list":
                return self.reply(200, {"jsonrpc": "2.0", "id": rid, "result": {"tools": [TOOL] + GH}})
            if method == "tools/call":
                args = req.get("params", {}).get("arguments", {})
                name = req.get("params", {}).get("name")
                with open("/tmp/mcp-calls", "a") as f:
                    auth = self.headers.get("Authorization", "")
                    f.write(json.dumps(args) + " tool=" + str(name) + " auth-sha=" + (hashlib.sha256(auth.encode()).hexdigest()[:16] if auth else "none") + "\n")
                text = job_logs() if name == "get_job_logs" else "echo: " + str(args.get("text"))
                return self.reply(200, {"jsonrpc": "2.0", "id": rid, "result": {
                    "content": [{"type": "text", "text": text}]}})
            if method == "ping":
                return self.reply(200, {"jsonrpc": "2.0", "id": rid, "result": {}})
            return self.reply(200, {"jsonrpc": "2.0", "id": rid, "error": {"code": -32601, "message": "no " + str(method)}})
        self.send_response(404)
        self.send_header("Content-Length", "0")
        self.end_headers()

    def log_message(self, *a):
        pass


class Server(ThreadingHTTPServer):
    address_family = socket.AF_INET6  # dual stack: siphon's guarded dial tries the first address, here IPv6


Server(("::", 8000), H).serve_forever()
