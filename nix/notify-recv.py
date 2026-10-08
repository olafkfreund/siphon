# A notification channel for the VM test: each POST body becomes one line of /tmp/notify.log.
import http.server


class H(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        body = self.rfile.read(int(self.headers.get("Content-Length", 0)))
        with open("/tmp/notify.log", "ab") as f:
            f.write(body.replace(b"\n", b" ") + b"\n")
        self.send_response(204)
        self.end_headers()


http.server.HTTPServer(("127.0.0.1", 18099), H).serve_forever()
