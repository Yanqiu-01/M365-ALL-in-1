# -*- coding: utf-8 -*-
"""Register / oauth 子脚本共享的配置读取。

子脚本以独立进程运行(cwd = 包根目录),通过这里取配置,
不在代码里写死站点、域名、密码和本机路径。
"""
from __future__ import annotations

import json
import os
import sys
from pathlib import Path
from typing import Any

BASE_DIR = Path(__file__).resolve().parent


def _config_path() -> Path:
    return Path(os.environ.get("M365_CONFIG") or (BASE_DIR / "config.json"))


def load() -> dict[str, Any]:
    path = _config_path()
    if not path.exists():
        example = BASE_DIR / "config.example.json"
        raise SystemExit(
            f"缺少配置文件 {path}\n"
            f"请复制 {example.name} 为 config.json 并填写 register 段。")
    return json.loads(path.read_text(encoding="utf-8"))


def register_config() -> dict[str, Any]:
    """注册相关配置。缺必填项直接报错退出,不带着空值往下跑。"""
    reg = load().get("register") or {}
    required = ("site_url", "turnstile_sitekey", "email_domain", "email_prefix", "password")
    missing = [k for k in required if not reg.get(k)]
    if missing:
        raise SystemExit(
            "config.json 的 register 段缺少必填项: " + ", ".join(missing))
    return reg


def gateway_url() -> str:
    gw = load().get("gateway") or {}
    host = gw.get("host") or "127.0.0.1"
    port = gw.get("port") or 4141
    return f"http://{host}:{port}"


def resolve(path_value: str | None, default: str = "") -> Path:
    """把配置里的相对路径按包根目录展开, ~ 也展开。"""
    raw = str(path_value or default)
    p = Path(raw).expanduser()
    return p if p.is_absolute() else (BASE_DIR / p)


def cred_file() -> Path:
    reg = load().get("register") or {}
    return resolve(reg.get("cred_file"), "data/credentials.txt")


def data_dir() -> Path:
    d = BASE_DIR / "data"
    d.mkdir(parents=True, exist_ok=True)
    return d


def append_credential(email: str, password: str) -> None:
    """注册成功后追加账密。只在成功路径调用。"""
    path = cred_file()
    path.parent.mkdir(parents=True, exist_ok=True)
    with path.open("a", encoding="utf-8") as f:
        f.write(f"{email}----{password}\n")


def find_cloak_chrome() -> str:
    """定位反指纹浏览器可执行文件(注册页的 Turnstile 需要它)。"""
    import glob
    reg = load().get("register") or {}
    pattern = str(reg.get("cloak_browser_glob") or "~/.cloakbrowser/chromium-*/chrome.exe")
    matches = sorted(glob.glob(str(Path(pattern).expanduser())))
    if not matches:
        raise SystemExit(
            f"找不到浏览器: {pattern}\n请安装 CloakBrowser 或修正 register.cloak_browser_glob")
    return matches[-1]


def admin_password() -> str:
    """网关管理员密码 —— 环境变量优先,其次 data_dir 下的文件。"""
    cfg = load()
    env_name = ((cfg.get("auth") or {}).get("admin_password_env")) or "M365_ADMIN_PASSWORD"
    if os.environ.get(env_name):
        return os.environ[env_name]
    gw_dir = Path(str((cfg.get("gateway") or {}).get("data_dir")
                      or "~/.config/m365-gateway")).expanduser()
    try:
        return (gw_dir / "admin-password").read_text(encoding="utf-8").strip()
    except Exception:
        raise SystemExit("读不到管理员密码,先运行 python run.py --bootstrap")


def load_credentials() -> dict[str, str]:
    path = cred_file()
    out: dict[str, str] = {}
    if path.exists():
        for line in path.read_text(encoding="utf-8", errors="replace").splitlines():
            if "----" in line:
                e, p = line.strip().split("----", 1)
                out[e.strip()] = p.strip()
    return out
