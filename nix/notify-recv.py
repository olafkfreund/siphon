# A notification channel for the VM test: each POST body becomes one line of /tmp/notify.log.
# POST /n and /k keep the raw body. Any other path is logged as
# {"event":"cmd","path":...,"body":...} (the demo's /kuma and /digest).
# GET / serves the log as text/plain (the demo's digest fetches it).
import http.server
import json
import os


class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        try:
            b = open("/tmp/notify.log", "rb").read()
        except OSError:
            b = b""
        self.send_response(200)
        self.send_header("Content-Type", "text/plain")
        self.send_header("Content-Length", str(len(b)))
        self.end_headers()
        self.wfile.write(b)

    def do_POST(self):
        body = self.rfile.read(int(self.headers.get("Content-Length", 0)))
        if self.path not in ("/n", "/k"):
            body = json.dumps({"event": "cmd", "path": self.path, "body": body.decode("utf-8", "replace")}).encode()
        with open("/tmp/notify.log", "ab") as f:
            f.write(body.replace(b"\n", b" ") + b"\n")
        self.send_response(204)
        self.end_headers()


http.server.HTTPServer((os.environ.get("NOTIFY_BIND", "127.0.0.1"), 18099), H).serve_forever()
