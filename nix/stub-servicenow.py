# VM test stand-in for a ServiceNow instance (stdlib only).
#   POST  /oauth_token.do                -> client-credentials token, if the
#                                           id/secret match the env
#   PATCH /api/now/table/incident/<id>   -> records the call, answers an incident
#   GET   /_seen                         -> the recorded PATCH calls
# Records hold {method, path, body, bearer_ok}, never the token or the secret.
# Env: STUB_SN_CLIENT_ID, STUB_SN_CLIENT_SECRET, optional STUB_SN_PORT.
import json
import os
import re
import socket
import urllib.parse
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

CLIENT_ID = os.environ["STUB_SN_CLIENT_ID"]
CLIENT_SECRET = os.environ["STUB_SN_CLIENT_SECRET"]
TOKEN = "stub-tok-0123456789abcdef"
INCIDENT = re.compile(r"^/api/now/table/incident/([0-9a-f]{32})$")
SEEN = []


class H(BaseHTTPRequestHandler):
    def reply(self, code, body):
        b = json.dumps(body).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(b)))
        self.end_headers()
        self.wfile.write(b)

    def body(self):
        return self.rfile.read(int(self.headers.get("Content-Length", 0))).decode()

    def do_GET(self):
        if self.path == "/_seen":
            return self.reply(200, SEEN)
        self.reply(404, {"error": "not found"})

    def do_POST(self):
        raw = self.body()
        if self.path != "/oauth_token.do":
            return self.reply(404, {"error": "not found"})
        form = urllib.parse.parse_qs(raw)
        auth = self.headers.get("Authorization", "")
        cid, sec = (form.get("client_id") or [""])[0], (form.get("client_secret") or [""])[0]
        if auth.startswith("Basic "):  # the client may send them as Basic auth instead
            import base64
            try:
                cid, _, sec = base64.b64decode(auth[6:]).decode().partition(":")
            except Exception:
                pass
        if (form.get("grant_type") or [""])[0] != "client_credentials" or cid != CLIENT_ID or sec != CLIENT_SECRET:
            return self.reply(401, {"error": "invalid_client"})
        self.reply(200, {"access_token": TOKEN, "token_type": "Bearer", "expires_in": 1800})

    def do_PATCH(self):
        raw = self.body()
        m = INCIDENT.match(self.path)
        if not m:
            return self.reply(404, {"error": "not found"})
        ok = self.headers.get("Authorization", "") == "Bearer " + TOKEN
        try:
            body = json.loads(raw or "null")
        except ValueError:
            body = raw
        SEEN.append({"method": "PATCH", "path": self.path, "body": body, "bearer_ok": ok})
        if not ok:
            return self.reply(401, {"error": {"message": "User Not Authenticated"}})
        self.reply(200, {"result": {"number": "INC0000001", "sys_id": m.group(1)}})

    def log_message(self, *a):
        pass


class Server(ThreadingHTTPServer):
    address_family = socket.AF_INET6  # dual stack, like stub-model


Server(("::", int(os.environ.get("STUB_SN_PORT", "8081"))), H).serve_forever()
