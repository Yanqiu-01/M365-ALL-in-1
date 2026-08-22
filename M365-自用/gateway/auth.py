# -*- coding: utf-8 -*-
"""PKCE 授权 + 账号存储(refresh token 落盘加密)。"""
from __future__ import annotations

import base64
import hashlib
import json
import os
import secrets
import threading
import time
import urllib.parse
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

import httpx

from .protocol import (
    AUTHORIZE_URL, CLIENT_ID, REDIRECT_URI, SCOPES, TOKEN_URL, TOKEN_USER_AGENT,
)

SESSION_TTL = 600.0  # 待回调的授权会话存活时间


def _b64url(data: bytes) -> str:
    return base64.urlsafe_b64encode(data).rstrip(b"=").decode("ascii")


def _jwt_claims(token: str) -> dict[str, Any]:
    """只读 JWT 载荷取 oid/tid/email,不验签(签名由上游保证)。"""
    try:
        payload = token.split(".")[1]
        payload += "=" * (-len(payload) % 4)
        return json.loads(base64.urlsafe_b64decode(payload))
    except Exception:
        return {}


class AuthError(RuntimeError):
    pass


# ── 待回调会话 ───────────────────────────────────────────
@dataclass
class _Pending:
    state: str
    verifier: str
    created: float


class SessionStore:
    def __init__(self) -> None:
        self._items: dict[str, _Pending] = {}
        self._lock = threading.Lock()

    def create(self) -> tuple[str, str]:
        verifier = _b64url(secrets.token_bytes(64))
        state = _b64url(secrets.token_bytes(16))
        with self._lock:
            self._sweep()
            self._items[state] = _Pending(state, verifier, time.time())
        challenge = _b64url(hashlib.sha256(verifier.encode("ascii")).digest())
        return state, challenge

    def take(self, state: str) -> str:
        """取出并作废(授权码一次性,无论成败会话都不该留)。"""
        with self._lock:
            self._sweep()
            item = self._items.pop(state, None)
        if item is None:
            raise AuthError("state 不存在或已过期,请重新开始授权")
        return item.verifier

    def _sweep(self) -> None:
        now = time.time()
        for k in [k for k, v in self._items.items() if now - v.created > SESSION_TTL]:
            self._items.pop(k, None)


def authorize_url(state: str, challenge: str, force_login: bool = False) -> str:
    params = {
        "client_id": CLIENT_ID,
        "response_type": "code",
        "redirect_uri": REDIRECT_URI,
        "response_mode": "query",
        "scope": " ".join(SCOPES),
        "state": state,
        "code_challenge": challenge,
        "code_challenge_method": "S256",
    }
    if force_login:
        params["prompt"] = "login"
    return AUTHORIZE_URL + "?" + urllib.parse.urlencode(params)


def parse_callback(callback: str) -> dict[str, str]:
    """容忍完整 URL / 裸 query / ?code=... / #code=... / 裸 code 几种形态。"""
    text = (callback or "").strip().strip('"').strip("'")
    if not text:
        raise AuthError("回调为空")
    if "code=" not in text and "error=" not in text:
        return {"code": text}  # 裸授权码
    if "#" in text and "?" not in text:
        text = text.replace("#", "?", 1)
    query = urllib.parse.urlsplit(text).query or text.lstrip("?")
    params = dict(urllib.parse.parse_qsl(query))
    if params.get("error"):
        raise AuthError(f"{params['error']}: {params.get('error_description', '')}".strip(": "))
    if not params.get("code"):
        raise AuthError("回调里没有 code 参数")
    return params


# ── token 兑换 ───────────────────────────────────────────
async def _post_token(form: dict[str, str]) -> dict[str, Any]:
    headers = {
        # 不要发 Origin: 会触发 AADSTS9002326
        "Content-Type": "application/x-www-form-urlencoded",
        "User-Agent": TOKEN_USER_AGENT,
        "Accept": "application/json",
    }
    async with httpx.AsyncClient(timeout=30) as client:
        r = await client.post(TOKEN_URL, data=form, headers=headers)
    try:
        payload = r.json()
    except ValueError:
        raise AuthError(f"token 端点返回非 JSON (HTTP {r.status_code})")
    if r.status_code != 200 or "access_token" not in payload:
        code = payload.get("error", f"HTTP {r.status_code}")
        raise AuthError(f"{code}: {payload.get('error_description', '')}".strip(": "))
    return payload


async def exchange_code(code: str, verifier: str) -> dict[str, Any]:
    return await _post_token({
        "client_id": CLIENT_ID,
        "grant_type": "authorization_code",
        "code": code,
        "redirect_uri": REDIRECT_URI,
        "code_verifier": verifier,
        "scope": " ".join(SCOPES),
    })


async def refresh_token(token: str) -> dict[str, Any]:
    return await _post_token({
        "client_id": CLIENT_ID,
        "grant_type": "refresh_token",
        "refresh_token": token,
        "scope": " ".join(SCOPES),
    })


# ── 账号 ─────────────────────────────────────────────────
@dataclass
class Account:
    id: str
    email: str = ""
    display_name: str = ""
    oid: str = ""
    tid: str = ""
    access_token: str = ""
    refresh_token: str = ""
    expires_at: float = 0.0
    updated_at: float = field(default_factory=time.time)
    disabled: bool = False
    last_error: str = ""

    @property
    def expired(self) -> bool:
        return time.time() >= self.expires_at - 120  # 留 2 分钟余量

    def public(self) -> dict[str, Any]:
        """对外视图 —— 绝不含 token。"""
        return {
            "id": self.id, "email": self.email, "displayName": self.display_name,
            "oid": self.oid, "tid": self.tid,
            "status": "disabled" if self.disabled else ("expired" if self.expired else "online"),
            "expiresAt": int(self.expires_at), "updatedAt": int(self.updated_at),
            "lastError": self.last_error,
        }


def account_from_tokens(payload: dict[str, Any]) -> Account:
    claims = _jwt_claims(payload.get("id_token") or payload.get("access_token") or "")
    oid = str(claims.get("oid") or "")
    tid = str(claims.get("tid") or "")
    email = str(claims.get("preferred_username") or claims.get("upn") or claims.get("email") or "")
    return Account(
        id=oid or email or secrets.token_hex(8),
        email=email,
        display_name=str(claims.get("name") or ""),
        oid=oid, tid=tid,
        access_token=str(payload.get("access_token") or ""),
        refresh_token=str(payload.get("refresh_token") or ""),
        expires_at=time.time() + float(payload.get("expires_in") or 3600),
    )


class AccountStore:
    """账号持久化。refresh token 用 data_dir 下的密钥加密。

    密钥文件丢了,已存的 token 就永远解不开 —— 所以密钥只在缺失时创建,绝不轮换。
    """

    def __init__(self, data_dir: Path) -> None:
        self.dir = Path(data_dir).expanduser()
        self.dir.mkdir(parents=True, exist_ok=True)
        self.path = self.dir / "accounts.json"
        self._key_path = self.dir / "store.key"
        self._lock = threading.Lock()
        self._items: dict[str, Account] = {}
        self._fernet = self._load_cipher()
        self._load()

    def _load_cipher(self):
        try:
            from cryptography.fernet import Fernet
        except ImportError:
            return None  # 无 cryptography 则明文存, 启动时会告警
        if self._key_path.exists():
            key = self._key_path.read_bytes().strip()
        else:
            key = Fernet.generate_key()
            self._key_path.write_bytes(key)
            try:
                os.chmod(self._key_path, 0o600)
            except Exception:
                pass
        return Fernet(key)

    def _enc(self, text: str) -> str:
        if not text or self._fernet is None:
            return text
        return "enc:" + self._fernet.encrypt(text.encode()).decode()

    def _dec(self, text: str) -> str:
        if not text.startswith("enc:"):
            return text
        if self._fernet is None:
            return ""
        try:
            return self._fernet.decrypt(text[4:].encode()).decode()
        except Exception:
            return ""

    def _load(self) -> None:
        if not self.path.exists():
            return
        try:
            raw = json.loads(self.path.read_text(encoding="utf-8"))
        except Exception:
            return
        for rec in raw.get("accounts", []):
            try:
                acc = Account(
                    id=rec["id"], email=rec.get("email", ""),
                    display_name=rec.get("displayName", ""),
                    oid=rec.get("oid", ""), tid=rec.get("tid", ""),
                    access_token="",  # access token 不落盘,靠 refresh 重取
                    refresh_token=self._dec(rec.get("refreshToken", "")),
                    expires_at=0.0,
                    updated_at=rec.get("updatedAt", time.time()),
                    disabled=rec.get("disabled", False),
                )
            except KeyError:
                continue
            if acc.refresh_token:
                self._items[acc.id] = acc

    def _save(self) -> None:
        data = {"accounts": [{
            "id": a.id, "email": a.email, "displayName": a.display_name,
            "oid": a.oid, "tid": a.tid,
            "refreshToken": self._enc(a.refresh_token),
            "updatedAt": int(a.updated_at), "disabled": a.disabled,
        } for a in self._items.values()]}
        tmp = self.path.with_suffix(".tmp")
        tmp.write_text(json.dumps(data, ensure_ascii=False, indent=2), encoding="utf-8")
        tmp.replace(self.path)

    def upsert(self, acc: Account) -> Account:
        with self._lock:
            existing = self._items.get(acc.id)
            if existing is None and acc.email:
                for a in self._items.values():
                    if a.email == acc.email:
                        existing = a
                        break
            if existing is not None:
                acc.id = existing.id
            self._items[acc.id] = acc
            acc.updated_at = time.time()
            self._save()
        return acc

    def all(self) -> list[Account]:
        with self._lock:
            return list(self._items.values())

    def get(self, account_id: str) -> Account | None:
        with self._lock:
            return self._items.get(account_id)

    def remove(self, account_id: str) -> bool:
        with self._lock:
            if self._items.pop(account_id, None) is None:
                return False
            self._save()
            return True

    def mark(self, account_id: str, *, disabled: bool | None = None, error: str = "") -> None:
        with self._lock:
            acc = self._items.get(account_id)
            if acc is None:
                return
            if disabled is not None:
                acc.disabled = disabled
            acc.last_error = error
            self._save()

    async def usable(self, account_id: str | None = None) -> Account:
        """取一个可用账号(必要时刷新 access token)。"""
        pool = [a for a in self.all() if not a.disabled and a.refresh_token]
        if account_id:
            pool = [a for a in pool if a.id == account_id or a.email == account_id]
        if not pool:
            raise AuthError("没有可用账号,请先在面板里完成 OAuth 授权")
        # 优先用 token 还没过期的, 避免每次都刷
        pool.sort(key=lambda a: (a.expired, a.updated_at))
        acc = pool[0]
        if acc.expired or not acc.access_token:
            try:
                payload = await refresh_token(acc.refresh_token)
            except AuthError as e:
                self.mark(acc.id, error=str(e))
                raise
            acc.access_token = str(payload.get("access_token") or "")
            if payload.get("refresh_token"):
                acc.refresh_token = str(payload["refresh_token"])
            acc.expires_at = time.time() + float(payload.get("expires_in") or 3600)
            claims = _jwt_claims(acc.access_token)
            acc.oid = acc.oid or str(claims.get("oid") or "")
            acc.tid = acc.tid or str(claims.get("tid") or "")
            self.upsert(acc)
        return acc
