"""Start an MCP server over stdio and assert tools/list returns tools.
Usage: aws-mcp-smoke.py <command> [args...]. No network needed."""
import json, subprocess, sys

p = subprocess.Popen(sys.argv[1:], stdin=subprocess.PIPE, stdout=subprocess.PIPE, text=True)

def send(msg):
    p.stdin.write(json.dumps(msg) + "\n")
    p.stdin.flush()

def reply(want):
    for line in p.stdout:
        msg = json.loads(line)
        if msg.get("id") == want:
            return msg
    sys.exit("server exited before replying")

send({"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": {
    "protocolVersion": "2025-06-18", "capabilities": {},
    "clientInfo": {"name": "smoke", "version": "1"}}})
reply(1)
send({"jsonrpc": "2.0", "method": "notifications/initialized"})
send({"jsonrpc": "2.0", "id": 2, "method": "tools/list"})
tools = reply(2)["result"]["tools"]
p.kill()
assert tools, "no tools"
print(sys.argv[1], len(tools), "tools:", ", ".join(t["name"] for t in tools))
