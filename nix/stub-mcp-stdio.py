# VM test stand-in for a stdio MCP server that needs a secret in its env.
# Tool "whoami" answers with a hash of STUB_TOKEN (proving the server got
# it, without echoing it) and the uid it runs as.
import hashlib
import json
import os
import sys

tok = os.environ.get("STUB_TOKEN", "")
digest = hashlib.sha256(tok.encode()).hexdigest()[:16] if tok else "missing"

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
        res = {"content": [{"type": "text", "text": "token-sha=%s uid=%d" % (digest, os.getuid())}]}
    elif m == "ping":
        res = {}
    else:
        print(json.dumps({"jsonrpc": "2.0", "id": rid, "error": {"code": -32601, "message": "no " + str(m)}}), flush=True)
        continue
    print(json.dumps({"jsonrpc": "2.0", "id": rid, "result": res}), flush=True)
