# -*- coding: utf-8 -*-
"""配置加载。所有本机相关的值都从 config.json / 环境变量来,代码里不留硬编码。"""
from __future__ import annotations

import json
import os
import secrets
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

BASE_DIR = Path(__file__).resolve().parent.parent
CONFIG_PATH = BASE_DIR / "config.json"
EXAMPLE_PATH = BASE_DIR / "config.example.json"

MIN_ADMIN_PASSWORD_LEN = 16


@dataclass
class Settings:
    host: str = "127.0.0.1"
    port: int = 4141
    data_dir: Path = field(default_factory=lambda: Path("~/.config/m365-gateway").expanduser())
    chat_timeout_seconds: float = 900.0
    chat_read_timeout_seconds: float = 300.0
    admin_password: str = ""
    api_key: str = ""
    panel_host: str = "127.0.0.1"
    panel_port: int = 8555
    register: dict[str, Any] = field(default_factory=dict)

    @property
    def gateway_url(self) -> str:
        return f"http://{self.host}:{self.port}"


def _raw_config() -> dict[str, Any]:
    if CONFIG_PATH.exists():
        return json.loads(CONFIG_PATH.read_text(encoding="utf-8"))
    if EXAMPLE_PATH.exists():
        return json.loads(EXAMPLE_PATH.read_text(encoding="utf-8"))
    return {}


def load_settings() -> Settings:
    raw = _raw_config()
    gw = raw.get("gateway", {})
    panel = raw.get("panel", {})
    auth = raw.get("auth", {})

    data_dir = Path(str(gw.get("data_dir") or "~/.config/m365-gateway")).expanduser()
    s = Settings(
        host=str(gw.get("host") or "127.0.0.1"),
        port=int(gw.get("port") or 4141),
        data_dir=data_dir,
        chat_timeout_seconds=float(gw.get("chat_timeout_seconds") or 900),
        chat_read_timeout_seconds=float(gw.get("chat_read_timeout_seconds") or 300),
        panel_host=str(panel.get("host") or "127.0.0.1"),
        panel_port=int(panel.get("port") or 8555),
        register=raw.get("register", {}) or {},
    )
    # 凭据优先环境变量, 其次 data_dir 下的文件(由 --bootstrap 写入)。不进 config.json。
    s.admin_password = (
        os.environ.get(str(auth.get("admin_password_env") or "M365_ADMIN_PASSWORD"), "")
        or _read_secret(data_dir / "admin-password"))
    s.api_key = (
        os.environ.get(str(auth.get("api_key_env") or "M365_API_KEY"), "")
        or _read_secret(data_dir / "api-key"))
    return s


def _read_secret(path: Path) -> str:
    try:
        return path.read_text(encoding="utf-8").strip()
    except Exception:
        return ""


def bootstrap(admin_password: str = "", api_key: str = "") -> tuple[str, str]:
    """首次初始化: 生成/写入管理员密码与 API key,返回 (密码, key)。

    密码只写到 data_dir(权限 600),不写进 config.json,避免误提交。
    """
    s = load_settings()
    s.data_dir.mkdir(parents=True, exist_ok=True)
    pw = admin_password or secrets.token_urlsafe(18)
    if len(pw) < MIN_ADMIN_PASSWORD_LEN:
        raise SystemExit(f"管理员密码至少 {MIN_ADMIN_PASSWORD_LEN} 位(当前 {len(pw)} 位)")
    key = api_key or ("sk-m365-" + secrets.token_urlsafe(24))
    for path, value in ((s.data_dir / "admin-password", pw), (s.data_dir / "api-key", key)):
        path.write_text(value, encoding="utf-8")
        try:
            os.chmod(path, 0o600)
        except Exception:
            pass
    if not CONFIG_PATH.exists() and EXAMPLE_PATH.exists():
        CONFIG_PATH.write_text(EXAMPLE_PATH.read_text(encoding="utf-8"), encoding="utf-8")
    return pw, key
