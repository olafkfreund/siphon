# VM test stand-in for a model endpoint and an MCP server (stdlib only).
#   GET  /v1/models            -> one model
#   POST /v1/chat/completions  -> asks for the echo tool once, then answers
#                                 with the tool's result
#   POST /mcp                  -> minimal streamable-HTTP MCP: initialize,
#                                 tools/list (echo), tools/call
# Every MCP tools/call is appended to /tmp/mcp-calls so the test can see it.
import json
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

TOOL = {"name": "echo", "description": "Echo the text back",
        "inputSchema": {"type": "object", "properties": {"text": {"type": "string"}}, "required": ["text"]}}


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
            return self.reply(200, {"data": [{"id": "stub-model"}]})
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
            if tool_msgs:
                msg = {"role": "assistant", "content": "final: " + str(tool_msgs[-1].get("content"))}
            elif req.get("tools"):
                msg = {"role": "assistant", "content": None, "tool_calls": [{
                    "id": "call1", "type": "function",
                    "function": {"name": "mcp__extm__echo", "arguments": json.dumps({"text": "hi from the model"})}}]}
            else:
                msg = {"role": "assistant", "content": "plain answer"}
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
                return self.reply(200, {"jsonrpc": "2.0", "id": rid, "result": {"tools": [TOOL]}})
            if method == "tools/call":
                args = req.get("params", {}).get("arguments", {})
                with open("/tmp/mcp-calls", "a") as f:
                    f.write(json.dumps(args) + "\n")
                return self.reply(200, {"jsonrpc": "2.0", "id": rid, "result": {
                    "content": [{"type": "text", "text": "echo: " + str(args.get("text"))}]}})
            if method == "ping":
                return self.reply(200, {"jsonrpc": "2.0", "id": rid, "result": {}})
            return self.reply(200, {"jsonrpc": "2.0", "id": rid, "error": {"code": -32601, "message": "no " + str(method)}})
        self.send_response(404)
        self.send_header("Content-Length", "0")
        self.end_headers()

    def log_message(self, *a):
        pass


ThreadingHTTPServer(("0.0.0.0", 8000), H).serve_forever()
