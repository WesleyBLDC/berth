"""Proof 2: Playwright MCP drives a Berth launched Chrome through the token gated CDP proxy."""

import json
import os
import re
import subprocess
import sys
import tempfile
import urllib.error
import urllib.request

here = os.path.dirname(os.path.abspath(__file__))
spike = os.path.dirname(here)
tmp = tempfile.mkdtemp(prefix="berth-proof2-")
token_file = os.path.join(tmp, "token")

proxy = subprocess.Popen(
    [os.path.join(spike, "bin", "spike-proxy"), "-token-file", token_file],
    stdout=subprocess.PIPE,
    stderr=open(os.path.join(tmp, "proxy.log"), "w"),
    text=True,
)
info = {}
while len(info) < 3:
    line = proxy.stdout.readline()
    if not line:
        sys.exit("proxy exited early")
    k, _, v = line.strip().partition("=")
    info[k] = v
token = open(token_file).read().strip()
endpoint, page = info["CDP_ENDPOINT"], info["TEST_PAGE"]


def status(headers):
    try:
        return urllib.request.urlopen(urllib.request.Request(endpoint + "/json/version", headers=headers)).status
    except urllib.error.HTTPError as e:
        return e.code


results = {
    "no token": status({}),
    "wrong token": status({"X-Berth-Token": "nope"}),
    "right token": status({"X-Berth-Token": token}),
}

mcp = subprocess.Popen(
    ["npx", "-y", "@playwright/mcp@0.0.85", "--cdp-endpoint", endpoint, "--cdp-header", f"X-Berth-Token: {token}"],
    stdin=subprocess.PIPE,
    stdout=subprocess.PIPE,
    stderr=open(os.path.join(tmp, "mcp.log"), "w"),
    text=True,
)
next_id = 0


def rpc(method, params=None, notify=False):
    global next_id
    msg = {"jsonrpc": "2.0", "method": method, "params": params or {}}
    if not notify:
        next_id += 1
        msg["id"] = next_id
    mcp.stdin.write(json.dumps(msg) + "\n")
    mcp.stdin.flush()
    if notify:
        return None
    while True:
        reply = json.loads(mcp.stdout.readline())
        if reply.get("id") == next_id:
            if "error" in reply:
                raise RuntimeError(reply["error"])
            return reply["result"]


def text(result):
    return "\n".join(c.get("text", "") for c in result.get("content", []))


try:
    rpc(
        "initialize",
        {"protocolVersion": "2025-06-18", "capabilities": {}, "clientInfo": {"name": "berth-spike", "version": "0"}},
    )
    rpc("notifications/initialized", notify=True)
    rpc("tools/call", {"name": "browser_navigate", "arguments": {"url": page}})
    snap = text(rpc("tools/call", {"name": "browser_snapshot", "arguments": {}}))
    ref = re.search(r'button "Press me" \[ref=(\w+)\]', snap).group(1)
    click = rpc("tools/call", {"name": "browser_click", "arguments": {"element": "Press me button", "target": ref}})
    after = text(rpc("tools/call", {"name": "browser_snapshot", "arguments": {}}))
    results["navigate, snapshot, click"] = "Clicked through Berth" in after
    open(os.path.join(tmp, "snap.txt"), "w").write(snap + "\n=====\n" + after)
    print("snapshots:", os.path.join(tmp, "snap.txt"))
finally:
    mcp.terminate()
    proxy.terminate()
    proxy.wait(10)

methods = re.findall(r"method=(\S+)", open(os.path.join(tmp, "proxy.log")).read())
results["cdp methods seen by proxy"] = len(methods)
print(json.dumps(results, indent=2))
ok = (
    results["no token"] == 401
    and results["wrong token"] == 401
    and results["right token"] == 200
    and results["navigate, snapshot, click"]
)
sys.exit(0 if ok else 1)
