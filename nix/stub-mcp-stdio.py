# VM test stand-in for a stdio MCP server that needs a secret in its env.
# Tool "whoami" answers with a hash of STUB_TOKEN (proving the server got
# it, without echoing it) and the uid it runs as.
import base64
import hashlib
import http.client
import json
import os
import sys
import time
import urllib.parse

tok = os.environ.get("STUB_TOKEN", "")
digest = hashlib.sha256(tok.encode()).hexdigest()[:16] if tok else "missing"

def sha(name):
    v = os.environ.get(name, "")
    return hashlib.sha256(v.encode()).hexdigest()[:16] if v else "missing"

# As an AWS bridge: hashes of the keys it got (never the keys), and the
# settings that stop it falling back to other credentials.
aws = "aws-akid-sha=%s aws-secret-sha=%s aws-token-sha=%s aws-region=%s aws-imds-off=%s aws-config=%s aws-creds-file=%s" % (
    sha("AWS_ACCESS_KEY_ID"), sha("AWS_SECRET_ACCESS_KEY"), sha("AWS_SESSION_TOKEN"),
    os.environ.get("AWS_REGION", "missing"), os.environ.get("AWS_EC2_METADATA_DISABLED", "missing"),
    os.environ.get("AWS_CONFIG_FILE", "missing"), os.environ.get("AWS_SHARED_CREDENTIALS_FILE", "missing"))

def fetch(host, port, path):
    # Siphon's egress proxy only tunnels (CONNECT), as clients do for https.
    # Returns the body of a 2xx answer, or None on any failure.
    try:
        p = urllib.parse.urlsplit(os.environ.get("http_proxy", ""))
        auth = base64.b64encode(("%s:%s" % (urllib.parse.unquote(p.username or ""), urllib.parse.unquote(p.password or ""))).encode()).decode()
        c = http.client.HTTPConnection(p.hostname, p.port, timeout=10)
        c.set_tunnel(host, port, headers={"Proxy-Authorization": "Basic " + auth})
        c.request("GET", path)
        r = c.getresponse()
        return r.read().decode().strip() if 200 <= r.status < 300 else None
    except Exception:
        return None


def proxy_report():
    # Only with STUB_FETCH=stub-fetch-on. Never prints a variable's value.
    if os.environ.get("STUB_FETCH") != "stub-fetch-on":
        return ""
    body = fetch("external", 8080, "/")
    blocked = fetch("external", 8000, "/v1/models")  # 200 if reachable, so a leak shows
    return " proxy-env=%s fetch=%s fetch-blocked=%s" % (
        "set" if os.environ.get("http_proxy") else "missing",
        body if body is not None else "error",
        "ok" if blocked is not None else "refused")

for line in sys.stdin:
    try:
        req = json.loads(line)
    except ValueError:
        continue
    if "id" not in req:
        continue
    m, rid = req.get("method"), req["id"]
    if m == "initialize":
        res = {"protocolVersion": req.get("params", {}).get("protocolVersion", "2025-06-18"),
               "capabilities": {"tools": {}}, "serverInfo": {"name": "stub-stdio", "version": "1"}}
    elif m == "tools/list":
        res = {"tools": [{"name": "whoami", "description": "token hash and uid",
                          "inputSchema": {"type": "object", "properties": {}}}]}
    elif m == "tools/call":
        # Hold the run open so the VM test can inspect it while it's live.
        time.sleep(float(os.environ.get("STUB_SLOW", "0")))
        res = {"content": [{"type": "text", "text": "token-sha=%s uid=%d %s%s" % (digest, os.getuid(), aws, proxy_report())}]}
    elif m == "ping":
        res = {}
    else:
        print(json.dumps({"jsonrpc": "2.0", "id": rid, "error": {"code": -32601, "message": "no " + str(m)}}), flush=True)
        continue
    print(json.dumps({"jsonrpc": "2.0", "id": rid, "result": res}), flush=True)
