# -*- coding: utf-8 -*-
"""
M365 注册 — 手机 LTE 轮换出口
============================
出口: 手机上的 SOCKS5 代理(如 Termux) → 移动网络
换 IP: adb 切飞行模式/数据开关, 直到出口 IP 变化(运营商 CGNAT)
站点限制: 按 geoip 限制来源地区; 每 IP 24h 仅成功 1 次

站点/域名/密码/SOCKS 地址全部来自 config.json 的 register 段。
需要 adb 且手机已授权调试。
用法:
  python register_m365_phone.py --limit 5    # 连续注册 N 个
  python register_m365_phone.py --only 5039  # 只注册指定编号
"""
import argparse, asyncio, json, subprocess, sys, time
from datetime import datetime, timezone, timedelta
from pathlib import Path
from urllib.parse import urlsplit

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))
import common

_REG = common.register_config()
SITE_URL = _REG["site_url"]
PLAN_ID = str(_REG.get("plan_id") or "1")
DOMAIN_ID = str(_REG.get("domain_id") or "1")
PASSWORD = _REG["password"]
EMAIL_PREFIX = _REG["email_prefix"]
EMAIL_DOMAIN = _REG["email_domain"]
START_NUM = int(_REG.get("email_start_num") or 1000)
DISPLAY_BASE = int(_REG.get("display_base") or 1)

SOCKS = str(_REG.get("phone_socks") or "socks5://127.0.0.1:1081")
_socks_parts = urlsplit(SOCKS)
SOCKS_HOST = _socks_parts.hostname or "127.0.0.1"
SOCKS_PORT = int(_socks_parts.port or 1081)
SITE_HOST = urlsplit(SITE_URL).hostname or ""
OUTPUT_DIR = common.data_dir()
CRED_FILE = common.cred_file()
STATE_FILE = OUTPUT_DIR / "phone_used_ips.json"
LOG_FILE = OUTPUT_DIR / "register_phone.log"
TZ = timezone(timedelta(hours=8))

def log(msg):
    line = f"[{datetime.now(TZ).strftime('%H:%M:%S')}] {msg}"
    print(line, flush=True)
    with open(LOG_FILE, "a", encoding="utf-8") as f:
        f.write(line + "\n")

def _run_limited(argv, timeout):
    proc = subprocess.Popen(argv, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)
    try:
        out, _ = proc.communicate(timeout=timeout)
        return out or "", proc.returncode
    except subprocess.TimeoutExpired:
        subprocess.run(["taskkill", "/F", "/T", "/PID", str(proc.pid)],
                       capture_output=True, text=True)
        return "", -9

def adb(*args):
    out, code = _run_limited(["adb"] + list(args), 60)
    return subprocess.CompletedProcess(["adb"] + list(args), code, stdout=out)

IP_ENDPOINTS = [("icanhazip.com", 80, "/"), ("api.ipify.org", 80, "/"),
                ("ifconfig.me", 80, "/ip"), ("ip.3322.net", 80, "/")]

import re as _re
_IPV4 = _re.compile(r"^(?:\d{1,3}\.){3}\d{1,3}$")

def _clean_ip(text):
    """从响应中提取合法 IPv4; 429/HTML 等垃圾一律判为无效"""
    if not text:
        return ""
    t = text.strip()
    if "<" in t or "html" in t.lower() or len(t) > 64:
        m = _re.search(r"(?:\d{1,3}\.){3}\d{1,3}", t)
        if not m:
            return ""
        t = m.group(0)
    t = t.strip().split()[0] if t.strip() else ""
    if _IPV4.match(t):
        parts = [int(x) for x in t.split(".")]
        if all(0 <= x <= 255 for x in parts):
            return t
    return ""

def socks_http_get(host, port, path, timeout=2.5):
    import socket, struct
    try:
        s = socket.create_connection((SOCKS_HOST, SOCKS_PORT), timeout=timeout)
        s.settimeout(timeout)
        s.sendall(b"\x05\x01\x00")
        if s.recv(2) != b"\x05\x00":
            s.close(); return None
        hb = host.encode()
        s.sendall(b"\x05\x01\x00\x03" + bytes([len(hb)]) + hb + struct.pack(">H", port))
        rep = s.recv(10)
        if len(rep) < 2 or rep[1] != 0:
            s.close(); return None
        s.sendall(f"GET {path} HTTP/1.0\r\nHost: {host}\r\nUser-Agent: probe/1\r\nConnection: close\r\n\r\n".encode())
        data = b""
        while True:
            chunk = s.recv(4096)
            if not chunk:
                break
            data += chunk
        s.close()
        return data.split(b"\r\n\r\n", 1)[-1].decode(errors="replace").strip()
    except Exception:
        return None

def curl_exit_ip(budget=12.0, timeout=2.5):
    """时间预算内快速轮询多个轻量端点; 只接受合法 IPv4"""
    t0 = time.time()
    while time.time() - t0 < budget:
        for host, port, path in IP_ENDPOINTS:
            ip = _clean_ip(socks_http_get(host, port, path, timeout))
            if ip:
                return ip
        time.sleep(0.4)
    return ""

def tcp_reachable(host, port=443, timeout=8):
    """经手机 SOCKS5 验证目标站点真的可连(避免断网窗口内发请求)"""
    import socket as _s, struct as _st
    try:
        c = _s.create_connection((SOCKS_HOST, SOCKS_PORT), timeout=timeout)
        c.settimeout(timeout)
        c.sendall(bytes([5, 1, 0]))
        if c.recv(2) != bytes([5, 0]):
            c.close(); return False
        hb = host.encode()
        c.sendall(bytes([5, 1, 0, 3]) + bytes([len(hb)]) + hb + _st.pack(">H", port))
        rep = c.recv(10)
        ok = len(rep) >= 2 and rep[1] == 0
        c.close()
        return ok
    except Exception:
        return False

def wait_site_ready(max_wait=180):
    """等注册站点经手机出口可连"""
    t0 = time.time()
    while time.time() - t0 < max_wait:
        if tcp_reachable(SITE_HOST, 443):
            return True
        time.sleep(6)
    return False

def ensure_net():
    ip = curl_exit_ip(budget=10.0)
    if ip:
        return ip
    log("  [net] 手机网络不通, 飞机模式恢复...")
    adb("shell", "cmd", "connectivity", "airplane-mode", "disable")
    adb("shell", "svc", "data", "enable")
    ip = curl_exit_ip(budget=30.0)
    if ip:
        return ip
    adb("shell", "cmd", "connectivity", "airplane-mode", "enable")
    time.sleep(3)
    adb("shell", "cmd", "connectivity", "airplane-mode", "disable")
    return curl_exit_ip(budget=60.0)

def load_used():
    if STATE_FILE.exists():
        try:
            return set(json.loads(STATE_FILE.read_text(encoding="utf-8")))
        except Exception:
            pass
    return set()

def save_used(used):
    STATE_FILE.write_text(json.dumps(sorted(used), ensure_ascii=False), encoding="utf-8")

def rotate_lte(prev_ip, max_rounds=6):
    """飞机模式 detach/re-attach 换 CGNAT 出口 IP。
    实测: svc data 只断数据, 移动经常分回同一出口IP(6轮4轮未变);
    飞机模式真正重新附着, IP 基本每次都变。耗时实测 8~30s。
    """
    for i in range(max_rounds):
        t0 = time.time()
        adb("shell", "cmd", "connectivity", "airplane-mode", "enable")
        time.sleep(3)
        adb("shell", "cmd", "connectivity", "airplane-mode", "disable")
        ip = curl_exit_ip(budget=60.0)
        if not ip:
            log(f"  [rotate {i+1}/{max_rounds}] 网络未恢复({time.time()-t0:.0f}s), 重来")
            continue
        if ip != prev_ip:
            log(f"  [rotate] {prev_ip or '(未知)'} -> {ip} ({time.time()-t0:.0f}s)")
            return ip
        log(f"  [rotate {i+1}/{max_rounds}] IP未变({ip}), 重来")
    return prev_ip

def append_cred(email):
    """写入账密文件(去重)。不打印密码。"""
    if email in common.load_credentials():
        log(f"  [cred] 已存在, 跳过 {email}")
        return
    common.append_credential(email, PASSWORD)
    log(f"  [cred] 已写入 {email}")

async def wait_token(page, timeout_s=50):
    deadline = time.time() + timeout_s
    while time.time() < deadline:
        try:
            tok = await page.evaluate(
                'document.querySelector("input[name=\\"cf-turnstile-response\\"]")?.value || ""')
            if tok and len(tok) > 10:
                return tok
        except Exception:
            pass
        await asyncio.sleep(0.8)
    return None

async def register_one(num, used_ips, browser):
    username = f"{EMAIL_PREFIX}{num}"
    email = f"{username}@{EMAIL_DOMAIN}"
    display = f"User{num - (START_NUM - DISPLAY_BASE)}"
    log(f"=== {email} | display={display} ===")
    ip = ensure_net()
    if not ip:
        log("  [fail] 手机网络不通")
        return {"num": num, "email": email, "status": "error", "message": "网络不通"}
    log(f"  出口IP={ip}")
    if not wait_site_ready(45):
        log("  [fail] 站点经手机出口不可连(仍在断网窗口)")
        return {"num": num, "email": email, "status": "error", "message": "站点不可连"}

    ctx = None
    try:
        if True:
            ctx = await browser.new_context(
                viewport={"width": 1280, "height": 900}, locale="zh-CN")
            page = await ctx.new_page()
            try:
                await page.goto(SITE_URL, timeout=60000, wait_until="domcontentloaded")
            except Exception as e:
                return {"num": num, "email": email, "status": "error", "message": f"打开页面失败: {e}"}

            try:
                await page.locator("#planId").wait_for(state="visible", timeout=25000)
                await page.locator("#planId").select_option(value=PLAN_ID)
            except Exception as e:
                log(f"  [warn] plan: {e}")
            try:
                await page.locator("#displayName").fill(display)
                await page.locator("#username").fill(username)
                await page.locator("#password").fill(PASSWORD)
            except Exception as e:
                return {"num": num, "email": email, "status": "error", "message": f"填表失败: {e}"}

            token = await wait_token(page, 60)
            if not token:
                try:
                    iframe = page.locator("iframe[src*='challenges.cloudflare.com']").first
                    box = await iframe.bounding_box(timeout=15000)
                    if box:
                        x = box["x"] + box["width"]/2
                        y = box["y"] + box["height"]/2
                        await page.mouse.move(x, y, steps=8)
                        await page.mouse.click(x, y)
                        token = await wait_token(page, 45)
                except Exception as e:
                    log(f"  无交互式 iframe: {e}")
            if not token:
                try:
                    await page.screenshot(path=str(OUTPUT_DIR / f"phone_fail_{num}.png"))
                except Exception:
                    pass
                return {"num": num, "email": email, "status": "captcha_failed",
                        "message": "Turnstile 未产生 token"}

            log(f"  token ok (len={len(token)})")
            payload = {"planId": PLAN_ID, "domainId": DOMAIN_ID, "inviteCode": "",
                       "displayName": display, "username": username, "password": PASSWORD,
                       "verificationEmail": "", "emailCode": "", "turnstileToken": token}
            api_result = await page.evaluate("""async (payload) => {
                try {
                    const r = await fetch('/api/register', {
                        method:'POST', headers:{'Content-Type':'application/json'},
                        body: JSON.stringify(payload), cache:'no-store'});
                    const data = await r.json();
                    return {ok:r.ok, status:r.status, data:data};
                } catch(e) { return {ok:false, status:0, data:{message:String(e)}}; }
            }""", payload)
            data = api_result.get("data") or {}
            ok = api_result.get("ok") and data.get("ok", True)
            msg = data.get("message", f"HTTP {api_result.get('status')}")
            if ok:
                log(f"  ✓ 成功: {data.get('upn', email)} | {msg}")
                append_cred(email)
                used_ips.add(ip)
                save_used(used_ips)
                return {"num": num, "email": email, "status": "success", "ip": ip,
                        "upn": data.get("upn", email), "message": msg}
            log(f"  ✗ 失败: {msg}")
            if "上限" in msg:
                used_ips.add(ip)
                save_used(used_ips)
            return {"num": num, "email": email, "status": "failed", "ip": ip, "message": msg}
    finally:
        if ctx:
            try:
                await ctx.close()
            except Exception:
                pass

async def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--limit", type=int, default=1)
    ap.add_argument("--only", type=int, default=0)
    ap.add_argument("--start", type=int, default=0)
    a = ap.parse_args()
    used = load_used()
    if a.only:
        nums = [a.only]
    else:
        base = a.start or START_NUM
        nums = [base + i for i in range(a.limit)]
    results = []
    from playwright.async_api import async_playwright
    async with async_playwright() as pw:
        async def launch():
            return await pw.chromium.launch(
                executable_path=common.find_cloak_chrome(), headless=True,
                proxy={"server": SOCKS},
                args=["--no-sandbox", "--disable-blink-features=AutomationControlled",
                      "--disable-dev-shm-usage", "--lang=zh-CN"])

        browser = await launch()

        async def run_one(n):
            """浏览器意外挂掉时自动重开一次"""
            nonlocal browser
            try:
                return await register_one(n, used, browser)
            except Exception as e:
                log(f"  [browser] 异常, 重开浏览器: {str(e)[:120]}")
                try:
                    await browser.close()
                except Exception:
                    pass
                browser = await launch()
                return await register_one(n, used, browser)

        for num in nums:
            res = await run_one(num)
            # IP上限/验证码失败 -> 换IP重试同一账号(失败不消耗用户名)
            retry = 0
            while res.get("status") != "success" and retry < 3:
                retry += 1
                log(f"  [retry {retry}] 换IP后重试 {num}")
                rotate_lte(curl_exit_ip(budget=10.0))
                res = await run_one(num)
            results.append(res)
            if num != nums[-1]:
                log("  准备下一个账号: 换 IP")
                rotate_lte(curl_exit_ip(budget=10.0))
            await asyncio.sleep(0.3)

        try:
            await browser.close()
        except Exception:
            pass
    OUTPUT_DIR.joinpath("register_results_phone.json").write_text(
        json.dumps(results, ensure_ascii=False, indent=2), encoding="utf-8")
    ok = sum(1 for r in results if r.get("status") == "success")
    log(f"==== 完成: 成功 {ok}/{len(results)} ====")

if __name__ == "__main__":
    asyncio.run(main())

