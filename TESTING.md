# Testing proxysim against the Simulator — quick guide

A minimal, repeatable checklist. Assumes a booted simulator and Xcode CLT.
Ports: proxy `8888`, UI `8889`. Network service: `Wi-Fi` (swap for `Ethernet` if
that's what you're on: `networksetup -listallnetworkservices`).

> ⚠️ The one thing that bites: routing the sim sets the **system-wide** proxy, so
> **you must turn it back off when done** (Step 5) or your Mac's web traffic
> breaks. The stop block below does it; the `run.sh` option does it automatically.

---

## One-time setup

```bash
cd ~/Documents/git/proxysim
go build -o proxysim .
./proxysim -port 8888 -ca-dir ~/.proxysim &   # first run generates the CA; Ctrl-C or `kill %1` after
sleep 2 && kill %1 2>/dev/null                 # (just needed it to create ~/.proxysim/ca.crt)
./proxysim -trust -ca-dir ~/.proxysim          # trust the CA in the booted simulator
```

Re-run only `-trust` after a `simctl erase`.

---

## Each test session

**1. Start proxysim** — pick one:

```bash
./proxysim -ui -ca-dir ~/.proxysim -app br.com.bb.InvestimentosBB   # just your app
# or
./proxysim -ui -ca-dir ~/.proxysim -only-sim                        # the whole simulator
```

**2. Point the simulator's traffic at it** (this is the system-wide part):

```bash
networksetup -setsecurewebproxy Wi-Fi 127.0.0.1 8888
networksetup -setwebproxy       Wi-Fi 127.0.0.1 8888
```

**3. Open the UI:** http://127.0.0.1:8889

**4. Drive the app** in the simulator — tap around, or:

```bash
xcrun simctl launch booted br.com.bb.InvestimentosBB   # (re)launch to see startup calls
```

Rows appear live; click one for headers + decoded body.

**5. STOP — revert the proxy, then quit proxysim:**

```bash
networksetup -setsecurewebproxystate Wi-Fi off
networksetup -setwebproxystate       Wi-Fi off
# then Ctrl-C proxysim (or: kill %1)
networksetup -getsecurewebproxy Wi-Fi | head -1   # sanity: "Enabled: No"
```

---

## Handy

```bash
# find any installed app's bundle id
xcrun simctl listapps booted | plutil -convert json -o - - \
  | python3 -c 'import sys,json; [print(k,"—",v.get("CFBundleDisplayName") or v.get("CFBundleName","")) for k,v in json.load(sys.stdin).items()]'

# is a simulator booted?
xcrun simctl list devices booted
```

**Notes**
- `-app` captures the app's own `URLSession`/`CFNetwork` calls. `WKWebView`
  traffic won't show under `-app` (it runs in WebKit's networking process) —
  use `-only-sim` to see it.
- Non-matching traffic is tunnelled (still works), just hidden.

---

## Optional: one-shot script that auto-reverts

Save as `run.sh`, `chmod +x run.sh`, then `./run.sh br.com.bb.InvestimentosBB`.
It reverts the system proxy on exit no matter how you quit (Ctrl-C included).

```bash
#!/usr/bin/env bash
set -euo pipefail
BUNDLE="${1:-}"; PORT=8888; UIPORT=8889; SVC=Wi-Fi
cd "$(dirname "$0")"
go build -o proxysim .

filter=(-only-sim); [ -n "$BUNDLE" ] && filter=(-app "$BUNDLE")
./proxysim -port "$PORT" -ui -ui-port "$UIPORT" -ca-dir ~/.proxysim "${filter[@]}" &
PX=$!
cleanup() {
  networksetup -setsecurewebproxystate "$SVC" off 2>/dev/null || true
  networksetup -setwebproxystate       "$SVC" off 2>/dev/null || true
  kill "$PX" 2>/dev/null || true
}
trap cleanup EXIT INT TERM
sleep 1
networksetup -setsecurewebproxy "$SVC" 127.0.0.1 "$PORT"
networksetup -setwebproxy       "$SVC" 127.0.0.1 "$PORT"
echo "proxysim up. UI: http://127.0.0.1:$UIPORT   (Ctrl-C to stop and auto-revert the proxy)"
wait "$PX"
```
