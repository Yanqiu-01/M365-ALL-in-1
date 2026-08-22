# -*- coding: utf-8 -*-
"""唯一入口。

  python run.py --bootstrap     首次初始化(生成管理员密码 + API key)
  python run.py                 同时启动网关和面板,并打开浏览器
  python run.py --gateway-only  只启动网关
  python run.py --panel-only    只启动面板(需网关已在运行)

不打印任何密码/token —— 唯一例外是 --bootstrap 首次生成时必须显示一次。
"""
from __future__ import annotations

import argparse
import asyncio
import sys
import threading
import webbrowser
from pathlib import Path

BASE_DIR = Path(__file__).resolve().parent
sys.path.insert(0, str(BASE_DIR))

try:
    import uvicorn
except ImportError:
    sys.exit("缺少依赖: pip install -r requirements.txt")

from gateway.config import bootstrap, load_settings


def _serve(app, host: str, port: int, label: str) -> threading.Thread:
    cfg = uvicorn.Config(app, host=host, port=port, log_level="warning")
    server = uvicorn.Server(cfg)

    def run() -> None:
        asyncio.run(server.serve())

    t = threading.Thread(target=run, name=label, daemon=True)
    t.start()
    return t


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--bootstrap", action="store_true", help="初始化管理员密码与 API key")
    ap.add_argument("--admin-password", default="", help="指定管理员密码(至少 16 位)")
    ap.add_argument("--api-key", default="", help="指定客户端 API key")
    ap.add_argument("--gateway-only", action="store_true")
    ap.add_argument("--panel-only", action="store_true")
    ap.add_argument("--no-browser", action="store_true")
    args = ap.parse_args()

    if args.bootstrap:
        pw, key = bootstrap(args.admin_password, args.api_key)
        s = load_settings()
        print("初始化完成。以下凭据只显示这一次:")
        print(f"  管理员密码 : {pw}")
        print(f"  API key    : {key}")
        print(f"  存放位置   : {s.data_dir}")
        print("\n下一步: python run.py")
        return 0

    settings = load_settings()
    if not settings.admin_password or not settings.api_key:
        print("尚未初始化。先运行: python run.py --bootstrap")
        return 1

    threads = []
    if not args.panel_only:
        from gateway.app import create_app
        threads.append(_serve(create_app(settings), settings.host, settings.port, "gateway"))
        print(f"网关: {settings.gateway_url}  (OpenAI 兼容 {settings.gateway_url}/v1)")

    if not args.gateway_only:
        from panel.app import create_panel
        panel_url = f"http://{settings.panel_host}:{settings.panel_port}"
        threads.append(_serve(create_panel(settings), settings.panel_host,
                              settings.panel_port, "panel"))
        print(f"面板: {panel_url}")
        if not args.no_browser:
            try:
                webbrowser.open(panel_url)
            except Exception:
                pass

    if not threads:
        print("没有要启动的服务")
        return 1
    print("Ctrl+C 退出。")
    try:
        while any(t.is_alive() for t in threads):
            for t in threads:
                t.join(timeout=0.5)
    except KeyboardInterrupt:
        print("\n已退出。")
    return 0


if __name__ == "__main__":
    sys.exit(main())
