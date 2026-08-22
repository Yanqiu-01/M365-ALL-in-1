# -*- coding: utf-8 -*-
"""
M365 注册 — Clash 机场出口
=========================
出口策略: 通过 Clash/mihomo 外部控制接口轮换节点组内的节点
站点限制: 每 IP 24h 仅成功 1 次 → 每个节点每天只试 1 个账号
流程: 切节点 → 校验出口 IP → CloakBrowser+Playwright 过 Turnstile → POST 注册
成功: 追加到 config.json 里 register.cred_file 指定的账密文件

站点/域名/密码/Clash 地址全部来自 config.json 的 register 段。
用法:
  python register_m365_clash.py --limit 4    # 注册 4 个
  python register_m365_clash.py --only 5039  # 只注册指定编号
"""
import argparse, asyncio, json, sys, time
from datetime import datetime, timezone, timedelta
from pathlib import Path

import urllib.request
import urllib.parse

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))
import common

_REG = common.register_config()
SITE_URL = _REG["site_url"]
SITEKEY = _REG["turnstile_sitekey"]
PLAN_ID = str(_REG.get("plan_id") or "1")
DOMAIN_ID = str(_REG.get("domain_id") or "1")
PASSWORD = _REG["password"]
EMAIL_PREFIX = _REG["email_prefix"]
EMAIL_DOMAIN = _REG["email_domain"]
START_NUM = int(_REG.get("email_start_num") or 1000)
DISPLAY_BASE = int(_REG.get("display_base") or 1)

CLASH_API = str(_REG.get("clash_api") or "http://127.0.0.1:9097")
CLASH_SECRET = str(_REG.get("clash_secret") or "")
CLASH_GROUP = str(_REG.get("clash_group") or "")
CLASH_PROXY = str(_REG.get("clash_proxy") or "http://127.0.0.1:7897")

# 节点列表来自 config.json 的 register.clash_nodes:
#   [{"name": "<Clash 里的节点名>", "expect_ip": "<该节点的预期出口 IP, 可留空>"}]
# 站点按 geoip 限制来源地区, 且每 IP 24h 仅成功 1 次, 所以每个节点每天只试 1 个账号。
NODES = [n for n in (_REG.get("clash_nodes") or []) if n.get("name")]

OUTPUT_DIR = common.data_dir()
CRED_FILE = common.cred_file()
RESULT_FILE = OUTPUT_DIR / "register_results_v2.json"
LOG_FILE = OUTPUT_DIR / "register_v2.log"
TZ = timezone(timedelta(hours=8))

def log(msg):
    line = f"[{datetime.now(TZ).strftime('%H:%M:%S')}] {msg}"
    print(line, flush=True)
    with open(LOG_FILE, "a", encoding="utf-8") as f:
        f.write(line + "\n")

def clash_request(path, method="GET", body=None):
    req = urllib.request.Request(
        CLASH_API + urllib.parse.quote(path, safe="/"), method=method,
        headers={"Authorization": f"Bearer {CLASH_SECRET}",
                 "Content-Type": "application/json"})
    data = json.dumps(body).encode("utf-8") if body is not None else None
    with urllib.request.urlopen(req, data=data, timeout=10) as r:
        raw = r.read()
        return json.loads(raw.decode("utf-8")) if raw else {}

def switch_node(name):
    clash_request(f"/proxies/{CLASH_GROUP}", "PUT", {"name": name})

def get_exit_ip(timeout=15):
    req = urllib.request.Request("https://api.ipify.org")
    # 经 Clash 出口
    import urllib.request as u2
    opener = u2.build_opener(u2.ProxyHandler({"https": CLASH_PROXY}))
    with opener.open(req, timeout=timeout) as r:
        return r.read().decode().strip()

def append_cred(email):
    """写入账密文件(去重)。不打印密码。"""
    if email in common.load_credentials():
        log(f"  [cred] 已存在, 跳过 {email}")
        return
    common.append_credential(email, PASSWORD)
    log(f"  [cred] 已写入 {email}")

async def wait_token(page, timeout_s=45):
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

async def register_one(num, node):
    username = f"{EMAIL_PREFIX}{num}"
    email = f"{username}@{EMAIL_DOMAIN}"
    display = f"User{num - (START_NUM - DISPLAY_BASE)}"
    log(f"=== {email} | display={display} | node={node['name']} ===")
    res = {"num": num, "email": email, "node": node["name"],
           "exit_ip": "", "status": "error", "message": ""}
    try:
        switch_node(node["name"])
        await asyncio.sleep(3)
        ip = get_exit_ip()
        res["exit_ip"] = ip
        log(f"  出口IP={ip} (期望 {node['expect_ip']})")
        if ip != node["expect_ip"]:
            log("  [warn] 出口IP与预登记不一致，继续但记录")
    except Exception as e:
        res["message"] = f"切节点/取IP失败: {e}"
        log(f"  [fail] {res['message']}")
        return res

    from playwright.async_api import async_playwright
    async with async_playwright() as pw:
        browser = None
        try:
            browser = await pw.chromium.launch(
                executable_path=common.find_cloak_chrome(), headless=True,
                proxy={"server": CLASH_PROXY},
                args=["--no-sandbox", "--disable-blink-features=AutomationControlled",
                      "--disable-dev-shm-usage", "--lang=zh-CN"])
            ctx = await browser.new_context(
                viewport={"width": 1280, "height": 900}, locale="zh-CN",
                user_agent="Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36")
            page = await ctx.new_page()
            try:
                await page.goto(SITE_URL, timeout=40000, wait_until="domcontentloaded")
            except Exception as e:
                res["message"] = f"打开页面失败: {e}"
                return res

            try:
                await page.locator("#planId").wait_for(state="visible", timeout=20000)
                await page.locator("#planId").select_option(value=PLAN_ID)
            except Exception as e:
                log(f"  [warn] plan 选择: {e}")
            await page.locator("#displayName").fill(display)
            await page.locator("#username").fill(username)
            await page.locator("#password").fill(PASSWORD)

            # managed/auto 模式: token 直接进 hidden input, 无 iframe
            token = await wait_token(page, 60)
            if not token:
                # 交互式: 点 iframe 中心触发勾选
                try:
                    iframe = page.locator("iframe[src*='challenges.cloudflare.com']").first
                    box = await iframe.bounding_box(timeout=15000)
                    if box:
                        x = box["x"] + box["width"]/2
                        y = box["y"] + box["height"]/2
                        await page.mouse.move(x, y, steps=8)
                        await page.mouse.click(x, y)
                        log("  已点击 turnstile 复选框, 等待 token...")
                        token = await wait_token(page, 45)
                except Exception as e:
                    log(f"  无交互式 iframe 或点击异常: {e}")

            if not token:
                res["status"] = "captcha_failed"
                res["message"] = "Turnstile 未产生 token"
                try:
                    await page.screenshot(path=str(OUTPUT_DIR / f"v2_fail_{num}.png"))
                except Exception:
                    pass
                return res

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
            res["message"] = data.get("message", f"HTTP {api_result.get('status')}")
            if ok:
                res["status"] = "success"
                res["upn"] = data.get("upn", email)
                log(f"  ✓ 成功: {res['upn']} | {res['message']}")
                append_cred(email)
            else:
                res["status"] = "failed"
                log(f"  ✗ 失败: {res['message']}")
        finally:
            if browser:
                try:
                    await browser.close()
                except Exception:
                    pass
    return res

async def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--limit", type=int, default=1)
    ap.add_argument("--only", type=int, default=0)
    ap.add_argument("--node", type=int, default=0)
    a = ap.parse_args()

    if a.only:
        nums = [a.only]
    else:
        nums = [START_NUM + i for i in range(a.limit)]
    nums = nums[:len(NODES)]

    results = []
    for i, (num, node) in enumerate(zip(nums, NODES)):
        if a.node:
            node = NODES[a.node - 1]
        try:
            results.append(await register_one(num, node))
        except Exception as e:
            results.append({"num": num, "status": "error", "message": f"异常: {e}"})
            log(f"  [exception] {e}")
        await asyncio.sleep(2)

    RESULT_FILE.write_text(json.dumps(results, ensure_ascii=False, indent=2), encoding="utf-8")
    ok = sum(1 for r in results if r.get("status") == "success")
    log(f"==== 完成: 成功 {ok}/{len(results)} ====")

if __name__ == "__main__":
    asyncio.run(main())
