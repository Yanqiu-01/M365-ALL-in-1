# -*- coding: utf-8 -*-
"""
M365 注册 — 代理池出口
======================
CloakBrowser + Playwright 解决 Cloudflare Turnstile,按代理池轮换出口。
站点/域名/密码/代理文件全部来自 config.json 的 register 段。

用法:
  python register_accounts.py --limit 3    # 本次最多注册 3 个
  python register_accounts.py --dry-run    # 仅打印不执行
  python register_accounts.py --start 3    # 从第 3 个账号开始
  python register_accounts.py --only 5     # 只注册第 5 个账号
"""
from __future__ import annotations

import argparse
import asyncio
import glob
import json
import os
import random
import string
import sys
import time
from datetime import datetime, timezone, timedelta
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))
import common

# ── 配置(来自 config.json 的 register 段) ──
_REG = common.register_config()
SITE_URL = _REG["site_url"]
TURNSTILE_SITEKEY = _REG["turnstile_sitekey"]
PLAN_ID = str(_REG.get("plan_id") or "1")
DOMAIN_ID = str(_REG.get("domain_id") or "1")
PASSWORD = _REG["password"]
EMAIL_PREFIX = _REG["email_prefix"]
EMAIL_START_NUM = int(_REG.get("email_start_num") or 1000)
EMAIL_COUNT = int(_REG.get("email_count") or 10)
EMAIL_DOMAIN = _REG["email_domain"]

PROXY_FILE = common.resolve(_REG.get("proxy_file"), "data/proxies.txt")
OUTPUT_DIR = common.data_dir()
RESULT_FILE = OUTPUT_DIR / "register_results.json"
LOG_FILE = OUTPUT_DIR / "register.log"

TZ = timezone(timedelta(hours=8))

find_cloak_chrome = common.find_cloak_chrome


# ── 代理处理 ──
def load_proxies(path: Path) -> list[str]:
    """从 IP.txt 加载代理列表，格式: host:port:user:pass → http://user:pass@host:port"""
    proxies = []
    raw_lines = path.read_text(encoding="utf-8-sig").splitlines()
    for line in raw_lines:
        s = line.strip()
        if not s or s.startswith("#"):
            continue
        if "://" in s:
            proxies.append(s)
            continue
        parts = s.split(":")
        if len(parts) == 4:
            host, port, user, pw = parts
            proxies.append(f"http://{user}:{pw}@{host}:{port}")
        elif len(parts) == 2:
            proxies.append(f"http://{parts[0]}:{parts[1]}")
        else:
            print(f"[WARN] 无法解析代理行: {s[:60]}")
    return proxies


def parse_proxy_for_playwright(proxy_url: str) -> dict:
    """将 http://user:pass@host:port 解析为 Playwright proxy 参数格式"""
    # 去掉 http:// 前缀
    rest = proxy_url.split("://", 1)[-1]
    if "@" in rest:
        cred, hostpart = rest.rsplit("@", 1)
        if ":" in cred:
            user, pw = cred.split(":", 1)
        else:
            user, pw = cred, ""
        host, port = hostpart.rsplit(":", 1)
        return {"server": f"http://{host}:{port}", "username": user, "password": pw}
    else:
        host, port = rest.rsplit(":", 1)
        return {"server": f"http://{host}:{port}"}


# ── 账号生成 ──
def generate_accounts() -> list[dict]:
    """生成账号列表"""
    accounts = []
    pass  # using EMAIL_START_NUM directly
    for i in range(EMAIL_COUNT):
        num = EMAIL_START_NUM + i
        username = f"{EMAIL_PREFIX}{num}"
        email = f"{username}@{EMAIL_DOMAIN}"
        accounts.append({
            "index": i + 1,
            "username": username,
            "email": email,
            "password": PASSWORD,
            "display_name": f"User{num - 5026}",
        })
    return accounts


# ── 日志 ──
def log(msg: str):
    ts = datetime.now(TZ).strftime("%H:%M:%S")
    line = f"[{ts}] {msg}"
    print(line, flush=True)
    try:
        LOG_FILE.parent.mkdir(parents=True, exist_ok=True)
        with open(LOG_FILE, "a", encoding="utf-8") as f:
            f.write(line + "\n")
    except Exception:
        pass


# ── Turnstile 解决方案 (借鉴 grok-free-register) ──
async def wait_for_turnstile_render(page, timeout_ms=15000):
    """等待 Turnstile widget 渲染完成"""
    deadline = time.time() + timeout_ms / 1000
    while time.time() < deadline:
        try:
            loaded = await page.evaluate(
                "() => typeof window.turnstile !== 'undefined' && !!document.querySelector('.cf-turnstile')"
            )
            if loaded:
                return True
        except Exception:
            pass
        await asyncio.sleep(0.3)
    return False


async def get_turnstile_iframe(page):
    """获取 Turnstile iframe 元素"""
    # 尝试多种选择器
    selectors = [
        "iframe[src*='challenges.cloudflare.com']",
        "iframe[src*='turnstile']",
        ".cf-turnstile iframe",
        "#turnstileBox iframe",
    ]
    for sel in selectors:
        try:
            frame = page.locator(sel).first
            if await frame.count() > 0:
                return frame
        except Exception:
            continue
    return None


async def click_turnstile(page, max_attempts=5):
    """点击 Turnstile 复选框"""
    for attempt in range(max_attempts):
        try:
            # 方法1: 直接点击 .cf-turnstile 元素中心
            widget = page.locator(".cf-turnstile").first
            if await widget.count() > 0:
                box = await widget.bounding_box()
                if box:
                    x = box["x"] + box["width"] / 2
                    y = box["y"] + box["height"] / 2
                    # 模拟真实鼠标移动
                    await page.mouse.move(max(0, x - 20), max(0, y - 5))
                    await asyncio.sleep(0.1)
                    await page.mouse.move(x, y, steps=10)
                    await asyncio.sleep(0.05)
                    await page.mouse.down()
                    await asyncio.sleep(0.05)
                    await page.mouse.up()
                    log(f"  [turnstile] 点击 widget 中心 ({attempt+1}/{max_attempts})")
                    return True

            # 方法2: 点击 iframe
            iframe_el = await get_turnstile_iframe(page)
            if iframe_el:
                box = await iframe_el.bounding_box()
                if box:
                    x = box["x"] + box["width"] / 2
                    y = box["y"] + box["height"] / 2
                    await page.mouse.move(max(0, x - 15), max(0, y - 5))
                    await asyncio.sleep(0.1)
                    await page.mouse.move(x, y, steps=10)
                    await asyncio.sleep(0.05)
                    await page.mouse.down()
                    await asyncio.sleep(0.05)
                    await page.mouse.up()
                    log(f"  [turnstile] 点击 iframe 中心 ({attempt+1}/{max_attempts})")
                    return True

            # 方法3: 点击 turnstileBox
            box_el = page.locator("#turnstileBox").first
            if await box_el.count() > 0:
                box = await box_el.bounding_box()
                if box:
                    x = box["x"] + box["width"] / 2
                    y = box["y"] + box["height"] / 2
                    await page.mouse.move(x, y, steps=10)
                    await page.mouse.down()
                    await asyncio.sleep(0.05)
                    await page.mouse.up()
                    log(f"  [turnstile] 点击 turnstileBox 中心 ({attempt+1}/{max_attempts})")
                    return True

        except Exception as e:
            log(f"  [turnstile] 点击异常: {e}")
        await asyncio.sleep(1)
    return False


async def read_turnstile_token(page) -> str:
    """读取 Turnstile token"""
    try:
        token = await page.evaluate(
            'document.querySelector("input[name=\\"cf-turnstile-response\\"]")?.value || ""'
        )
        return token or ""
    except Exception:
        return ""


async def poll_turnstile_token(page, timeout_s=60, check_interval=1.0) -> str | None:
    """轮询等待 Turnstile token 生成"""
    deadline = time.time() + timeout_s
    while time.time() < deadline:
        token = await read_turnstile_token(page)
        if token and len(token) > 10:
            return token
        await asyncio.sleep(check_interval)
    return None


async def solve_turnstile(page, max_click_rounds=3) -> str | None:
    """
    解决 Turnstile 验证码的完整流程:
    1. 等待 Turnstile widget 渲染
    2. 点击触发挑战
    3. 轮询获取 token
    """
    # 等待页面加载 Turnstile
    rendered = await wait_for_turnstile_render(page, timeout_ms=15000)
    if not rendered:
        log("  [turnstile] widget 未渲染，可能不需要验证码或页面未完全加载")
        # 即使没渲染也尝试读取已有 token
        token = await read_turnstile_token(page)
        if token:
            return token
        return None

    log("  [turnstile] widget 已渲染，开始解决...")

    for round_idx in range(max_click_rounds):
        # 检查是否已有 token
        token = await read_turnstile_token(page)
        if token and len(token) > 10:
            log(f"  [turnstile] 第 {round_idx+1} 轮获取到 token (len={len(token)})")
            return token

        # 点击 Turnstile
        clicked = await click_turnstile(page, max_attempts=3)
        if clicked:
            # 等待 token 生成
            token = await poll_turnstile_token(page, timeout_s=20)
            if token:
                log(f"  [turnstile] 点击后获取到 token (len={len(token)})")
                return token

        await asyncio.sleep(1)

    # 最后尝试
    token = await read_turnstile_token(page)
    if token:
        log(f"  [turnstile] 最终获取到 token (len={len(token)})")
        return token

    log("  [turnstile] 未能获取 token")
    return None


# ── 注册主流程 ──
async def register_one_account(
    account: dict,
    proxy_url: str,
    chrome_path: str,
    dry_run: bool = False,
) -> dict:
    """
    注册单个账号:
    1. 使用代理启动 CloakBrowser
    2. 打开注册页面
    3. 填写表单
    4. 解决 Turnstile
    5. 提交注册
    """
    idx = account["index"]
    email = account["email"]
    username = account["username"]
    result = {
        "index": idx,
        "email": email,
        "username": username,
        "proxy": proxy_url.split("@")[-1] if "@" in proxy_url else proxy_url,
        "status": "pending",
        "message": "",
        "upn": "",
        "login_url": "",
        "timestamp": datetime.now(TZ).isoformat(timespec="seconds"),
    }

    if dry_run:
        log(f"[{idx}/{EMAIL_COUNT}] DRY RUN — {email} via {result['proxy']}")
        result["status"] = "dry_run"
        return result

    log(f"[{idx}/{EMAIL_COUNT}] 开始注册 {email} via {result['proxy']}")

    proxy_config = parse_proxy_for_playwright(proxy_url)

    from playwright.async_api import async_playwright

    async with async_playwright() as pw:
        # 使用 CloakBrowser 启动浏览器
        launch_args = [
            "--no-sandbox",
            "--disable-blink-features=AutomationControlled",
            "--disable-dev-shm-usage",
            "--lang=zh-CN",
        ]

        try:
            browser = await pw.chromium.launch(
                executable_path=chrome_path,
                headless=True,
                args=launch_args,
                proxy=proxy_config,
            )
        except Exception as e:
            # 如果 headless 失败，尝试非 headless
            log(f"  [browser] headless 启动失败: {e}，尝试非 headless...")
            try:
                browser = await pw.chromium.launch(
                    executable_path=chrome_path,
                    headless=False,
                    args=launch_args,
                    proxy=proxy_config,
                )
            except Exception as e2:
                log(f"  [browser] 启动失败: {e2}")
                result["status"] = "error"
                result["message"] = f"浏览器启动失败: {e2}"
                return result

        try:
            context = await browser.new_context(
                viewport={"width": 1280, "height": 800},
                locale="zh-CN",
                user_agent="Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36",
            )

            # 注入反检测脚本
            await context.add_init_script("""
                // 移除 webdriver 标记
                Object.defineProperty(navigator, 'webdriver', { get: () => undefined });
                // 修改 plugins
                Object.defineProperty(navigator, 'plugins', {
                    get: () => [1, 2, 3, 4, 5],
                });
                // 修改 languages
                Object.defineProperty(navigator, 'languages', {
                    get: () => ['zh-CN', 'zh', 'en'],
                });
            """)

            page = await context.new_page()

            # ── 步骤1: 打开注册页面 ──
            log("  [1/5] 打开注册页面...")
            try:
                await page.goto(SITE_URL, timeout=30000, wait_until="domcontentloaded")
            except Exception as e:
                log(f"  [1/5] 页面加载超时: {e}")
                result["status"] = "error"
                result["message"] = f"页面加载失败: {e}"
                return result

            # 等待页面初始化 (config + plans 加载)
            await asyncio.sleep(3)

            # ── 步骤2: 填写表单 ──
            log("  [2/5] 填写表单...")

            # 选择 plan (Office 365 E3)
            try:
                plan_select = page.locator("#planId")
                await plan_select.wait_for(state="visible", timeout=10000)
                await plan_select.select_option(value=PLAN_ID)
                await asyncio.sleep(0.5)
            except Exception as e:
                log(f"  [2/5] 选择 plan 失败: {e}")

            # 填写显示名称
            try:
                display_input = page.locator("#displayName")
                await display_input.wait_for(state="visible", timeout=10000)
                await display_input.fill(account["display_name"])
            except Exception as e:
                log(f"  [2/5] 填写显示名称失败: {e}")

            # 填写用户名 (邮箱前缀)
            try:
                username_input = page.locator("#username")
                await username_input.wait_for(state="visible", timeout=10000)
                await username_input.fill(username)
            except Exception as e:
                log(f"  [2/5] 填写用户名失败: {e}")

            # 填写密码
            try:
                password_input = page.locator("#password")
                await password_input.wait_for(state="visible", timeout=10000)
                await password_input.fill(PASSWORD)
            except Exception as e:
                log(f"  [2/5] 填写密码失败: {e}")

            await asyncio.sleep(0.5)

            # ── 步骤3: 解决 Turnstile 验证码 ──
            log("  [3/5] 解决 Turnstile 验证码...")
            turnstile_token = await solve_turnstile(page, max_click_rounds=3)

            if not turnstile_token:
                log("  [3/5] Turnstile 解决失败!")
                result["status"] = "captcha_failed"
                result["message"] = "Turnstile 验证码解决失败"
                # 截图保存
                try:
                    screenshot_path = OUTPUT_DIR / f"captcha_fail_{idx}.png"
                    await page.screenshot(path=str(screenshot_path))
                    log(f"  [3/5] 截图已保存: {screenshot_path}")
                except Exception:
                    pass
                return result

            log(f"  [3/5] Turnstile token 获取成功 (len={len(turnstile_token)})")

            # ── 步骤4: 提交注册 ──
            log("  [4/5] 提交注册...")

            # 方法A: 通过页面 JS 直接调用 API (更可靠)
            register_payload = {
                "planId": PLAN_ID,
                "domainId": DOMAIN_ID,
                "inviteCode": "",
                "displayName": account["display_name"],
                "username": username,
                "password": PASSWORD,
                "verificationEmail": "",
                "emailCode": "",
                "turnstileToken": turnstile_token,
            }

            try:
                api_result = await page.evaluate(
                    """async (payload) => {
                        try {
                            const r = await fetch('/api/register', {
                                method: 'POST',
                                headers: { 'Content-Type': 'application/json' },
                                body: JSON.stringify(payload),
                                cache: 'no-store'
                            });
                            const data = await r.json();
                            return { ok: r.ok, status: r.status, data: data };
                        } catch(e) {
                            return { ok: false, status: 0, data: { message: e.message } };
                        }
                    }""",
                    register_payload,
                )

                is_success = api_result.get("ok") and api_result.get("data", {}).get("ok", True)
                data = api_result.get("data", {})

                if is_success:
                    result["status"] = "success"
                    result["message"] = data.get("message", "注册成功")
                    result["upn"] = data.get("upn", email)
                    result["login_url"] = data.get("loginUrl", "https://portal.office.com")
                    log(f"  [4/5] ✓ 注册成功! UPN: {result['upn']}")
                else:
                    result["status"] = "failed"
                    result["message"] = data.get("message", f"HTTP {api_result.get('status')}")
                    log(f"  [4/5] ✗ 注册失败: {result['message']}")

            except Exception as e:
                log(f"  [4/5] API 调用异常: {e}")

                # 方法B: 回退到点击提交按钮
                log("  [4/5] 尝试通过按钮提交...")
                try:
                    submit_btn = page.locator("#submitBtn")
                    await submit_btn.click()
                    await asyncio.sleep(5)

                    # 检查结果
                    msg_el = page.locator("#message")
                    msg_class = await msg_el.get_attribute("class") or ""
                    msg_text = await msg_el.text_content() or ""

                    if "success" in msg_class:
                        result["status"] = "success"
                        result["message"] = msg_text
                        result["upn"] = email
                        log(f"  [4/5] ✓ 注册成功! (按钮提交)")
                    else:
                        result["status"] = "failed"
                        result["message"] = msg_text or "未知错误"
                        log(f"  [4/5] ✗ 注册失败: {msg_text}")
                except Exception as e2:
                    result["status"] = "error"
                    result["message"] = f"提交异常: {e2}"
                    log(f"  [4/5] ✗ 提交异常: {e2}")

            # ── 步骤5: 保存截图 ──
            log("  [5/5] 保存截图...")
            try:
                screenshot_path = OUTPUT_DIR / f"register_{idx}_{result['status']}.png"
                await page.screenshot(path=str(screenshot_path), full_page=True)
                log(f"  [5/5] 截图: {screenshot_path}")
            except Exception:
                pass

        finally:
            try:
                await context.close()
            except Exception:
                pass
            try:
                await browser.close()
            except Exception:
                pass

    result["timestamp"] = datetime.now(TZ).isoformat(timespec="seconds")
    return result


# ── 保存结果 ──
def save_results(results: list[dict]):
    OUTPUT_DIR.mkdir(parents=True, exist_ok=True)
    RESULT_FILE.write_text(
        json.dumps(results, ensure_ascii=False, indent=2) + "\n",
        encoding="utf-8",
    )
    # 同时保存成功账号列表
    success = [r for r in results if r.get("status") == "success"]
    # 追加到账密文件(面板与授权脚本都读它)
    for r in success:
        common.append_credential(r["email"], r["password"])
    log(f"结果已保存: {RESULT_FILE}")
    log(f"成功账号: {len(success)}/{len(results)} → {common.cred_file()}")


# ── 主函数 ──
async def main():
    parser = argparse.ArgumentParser(description="M365 注册 — 代理池出口")
    parser.add_argument("--dry-run", action="store_true", help="仅打印不执行")
    parser.add_argument("--start", type=int, default=1, help="从第几个账号开始 (1-based)")
    parser.add_argument("--only", type=int, default=0, help="只注册指定编号的账号")
    parser.add_argument("--headless", type=str, default="true", help="是否无头模式")
    parser.add_argument("--limit", type=int, default=0, help="本次最多注册几个 (0=不限)")
    args = parser.parse_args()

    # 加载代理
    if not PROXY_FILE.exists():
        log(f"错误: 代理文件不存在: {PROXY_FILE}")
        return 1
    proxies = load_proxies(PROXY_FILE)
    log(f"已加载 {len(proxies)} 个代理")

    # 生成账号
    accounts = generate_accounts()
    log(f"已生成 {len(accounts)} 个账号: {accounts[0]['email']} ~ {accounts[-1]['email']}")

    # 过滤账号
    if args.only > 0:
        accounts = [a for a in accounts if a["index"] == args.only]
        proxies = [proxies[args.only - 1]] if args.only <= len(proxies) else [proxies[0]]
    elif args.start > 1:
        accounts = [a for a in accounts if a["index"] >= args.start]
        proxies = proxies[args.start - 1:]

    if args.limit > 0:
        accounts = accounts[:args.limit]

    if len(proxies) < len(accounts):
        log(f"警告: 代理数({len(proxies)}) < 账号数({len(accounts)})，将循环使用代理")
        while len(proxies) < len(accounts):
            proxies.append(proxies[len(proxies) % len(proxies)])

    # 查找 CloakBrowser
    try:
        chrome_path = find_cloak_chrome()
        log(f"CloakBrowser: {chrome_path}")
    except RuntimeError as e:
        log(f"错误: {e}")
        return 1

    log("=" * 60)
    log(f"开始注册 {len(accounts)} 个账号")
    log(f"目标: {SITE_URL}")
    log(f"域名: {EMAIL_DOMAIN} (plan={PLAN_ID}, domain={DOMAIN_ID})")
    log("=" * 60)

    results = []

    for i, account in enumerate(accounts):
        proxy = proxies[i]

        result = await register_one_account(
            account,
            proxy,
            chrome_path,
            dry_run=args.dry_run,
        )
        results.append(result)

        # 保存中间结果
        save_results(results)

        # 注册间隔 (避免频率限制)
        if i < len(accounts) - 1 and not args.dry_run:
            wait_s = random.randint(5, 15)
            log(f"等待 {wait_s}s 后继续下一个账号...")
            await asyncio.sleep(wait_s)

    # 最终汇总
    log("=" * 60)
    log("注册完成!")
    success_count = sum(1 for r in results if r.get("status") == "success")
    fail_count = sum(1 for r in results if r.get("status") == "failed")
    error_count = sum(1 for r in results if r.get("status") == "error")
    captcha_fail = sum(1 for r in results if r.get("status") == "captcha_failed")
    log(f"成功: {success_count} | 失败: {fail_count} | 验证码失败: {captcha_fail} | 错误: {error_count}")
    log("=" * 60)

    return 0 if success_count > 0 else 1


if __name__ == "__main__":
    sys.exit(asyncio.run(main()))
