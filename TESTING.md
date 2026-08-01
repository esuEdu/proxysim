# Testing proxysim against the Simulator — quick guide

A minimal, repeatable checklist. Assumes a booted simulator and Xcode CLT.
Ports: proxy `8888`, UI `8889`.

With `-system-proxy`, proxysim now trusts the CA, sets the macOS system proxy, and
**restores it on exit** for you — so the old "remember to turn the proxy back off"
footgun is handled. The manual path is still here as a fallback.

---

## One-time setup

```bash
cd ~/Documents/git/proxysim
go build -o proxysim .
```

The first run generates the CA under `~/.proxysim` and auto-trusts it in the
booted simulator; there's no separate bootstrap step. (Re-run `./proxysim -trust`
only if you `simctl erase` a device and don't want to restart proxysim.)

---

## Each test session — the easy path

**1. Start proxysim (it trusts the CA, sets + later restores the system proxy):**

```bash
./proxysim -system-proxy -ui -ca-dir ~/.proxysim
```

**2. Open the UI:** http://127.0.0.1:8889

**3. Pick what to capture** in the control bar: the simulator, then *All simulator
traffic* or one app from the dropdown. No restart — it applies to the next
connection. (Prefer flags? add `-app br.com.bb.InvestimentosBB` or `-only-sim`.)

**4. Drive the app** in the simulator — tap around, or:

```bash
xcrun simctl launch booted br.com.bb.InvestimentosBB   # (re)launch to see startup calls
```

Rows appear live; click one for headers + decoded body.

**5. STOP:** just `Ctrl-C` proxysim. It restores the system proxy to its prior
state automatically. Sanity-check if you like:

```bash
networksetup -getsecurewebproxy Wi-Fi | head -1   # back to its previous "Enabled: …"
```

If a run is ever killed with `kill -9`, the next `./proxysim -system-proxy` startup
restores the system proxy from an on-disk snapshot before re-applying — so a stale
proxy never lingers.

---

## Each test session — the manual path (fallback)

If you'd rather not have proxysim touch the system proxy (e.g. you're on an unusual
network service), drop `-system-proxy` and drive it yourself. Network service:
`Wi-Fi` (swap for `Ethernet`: `networksetup -listallnetworkservices`).

```bash
./proxysim -ui -ca-dir ~/.proxysim -app br.com.bb.InvestimentosBB   # or -only-sim
networksetup -setsecurewebproxy Wi-Fi 127.0.0.1 8888
networksetup -setwebproxy       Wi-Fi 127.0.0.1 8888
# ... test in the UI ...
# ⚠️ revert when done, or your Mac's web traffic keeps routing through proxysim:
networksetup -setsecurewebproxystate Wi-Fi off
networksetup -setwebproxystate       Wi-Fi off
networksetup -getsecurewebproxy Wi-Fi | head -1   # sanity: "Enabled: No"
```

---

## Handy

```bash
# find any installed app's bundle id (or just pick it from the UI dropdown)
xcrun simctl listapps booted | plutil -convert json -o - - \
  | python3 -c 'import sys,json; [print(k,"—",v.get("CFBundleDisplayName") or v.get("CFBundleName","")) for k,v in json.load(sys.stdin).items()]'

# is a simulator booted?
xcrun simctl list devices booted
```

**Notes**
- `-app` (and the UI's per-app choice) capture the app's own
  `URLSession`/`CFNetwork` calls. `WKWebView` traffic won't show under a single
  app (it runs in WebKit's networking process) — use *All simulator traffic* /
  `-only-sim` to see it.
- Non-matching traffic is tunnelled (still works), just hidden.
- `-system-proxy` implies `-only-sim`, so host apps (Safari, daemons) are
  tunnelled untouched — never decrypted — even though the system proxy is global.
