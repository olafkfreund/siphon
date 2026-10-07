# VM test stand-in for AWS STS: answers AssumeRole with fixed temporary
# credentials and appends each request's form body to /tmp/sts-requests.
from http.server import BaseHTTPRequestHandler, HTTPServer
from urllib.parse import parse_qs

CREDS = """<AssumeRoleResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/">
<AssumeRoleResult><Credentials><AccessKeyId>ASIATEMPVMTEST</AccessKeyId>
<SecretAccessKey>temp-secret-vm-test</SecretAccessKey><SessionToken>temp-token-vm-test</SessionToken>
<Expiration>2099-01-01T00:00:00Z</Expiration></Credentials>
<AssumedRoleUser><Arn>arn:aws:sts::123456789012:assumed-role/siphon-test/siphon</Arn>
<AssumedRoleId>AROAVMTEST:siphon</AssumedRoleId></AssumedRoleUser></AssumeRoleResult>
<ResponseMetadata><RequestId>vm-test</RequestId></ResponseMetadata></AssumeRoleResponse>"""


class H(BaseHTTPRequestHandler):
    def do_POST(self):
        body = self.rfile.read(int(self.headers.get("Content-Length", 0))).decode()
        with open("/tmp/sts-requests", "a") as f:
            f.write(body + "\n")
        form = parse_qs(body)
        if form.get("Action") != ["AssumeRole"]:
            self.send_response(400)
            self.end_headers()
            return
        out = CREDS.encode()
        self.send_response(200)
        self.send_header("Content-Type", "text/xml")
        self.send_header("Content-Length", str(len(out)))
        self.end_headers()
        self.wfile.write(out)


HTTPServer(("127.0.0.1", 8099), H).serve_forever()
