# -*- coding: utf-8 -*-
"""M365 Copilot → OpenAI 兼容网关。

对外:
  GET  /v1/models                  模型目录(需 API key)
  POST /v1/chat/completions        流式/非流式(需 API key)
  GET  /api/auth/start             取授权 URL(需 admin)
  GET  /api/auth/callback          提交回调 code(需 admin)
  GET  /api/accounts               账号列表(需 admin)
  DELETE /api/accounts/{id}        删除账号(需 admin)
  POST /api/admin/login            管理员登录,置会话 cookie
  GET  /healthz                    存活探针(无鉴权)
"""
from __future__ import annotations

import asyncio
import json
import secrets
import time
import uuid
from typing import Any

from fastapi import Depends, FastAPI, HTTPException, Request, Response
from fastapi.responses import JSONResponse, StreamingResponse

from .auth import (
    Account, AccountStore, AuthError, SessionStore, account_from_tokens,
    authorize_url, exchange_code, parse_callback,
)
from .chathub import ChatHubError, ChatRequest, stream_chat
from .config import Settings
from .protocol import BASE_TONES, DEFAULT_MODEL, known_model, resolve_tone

SESSION_COOKIE = "m365_admin_session"


def create_app(settings: Settings) -> FastAPI:
    app = FastAPI(title="M365 Copilot 网关", docs_url=None, redoc_url=None)
    store = AccountStore(settings.data_dir)
    sessions = SessionStore()
    admin_sessions: dict[str, float] = {}

    # ── 鉴权 ─────────────────────────────────────────────
    def require_admin(request: Request) -> None:
        token = request.cookies.get(SESSION_COOKIE, "")
        expires = admin_sessions.get(token, 0)
        if not token or expires < time.time():
            admin_sessions.pop(token, None)
            raise HTTPException(401, {"message": "administrator login required",
                                      "type": "auth_error"})

    def require_api_key(request: Request) -> None:
        header = request.headers.get("authorization", "")
        key = header[7:].strip() if header.lower().startswith("bearer ") else ""
        key = key or request.headers.get("x-api-key", "")
        if not settings.api_key or not secrets.compare_digest(key, settings.api_key):
            raise HTTPException(401, {"message": "invalid api key", "type": "auth_error"})

    @app.get("/healthz")
    def healthz() -> dict[str, Any]:
        accounts = store.all()
        return {
            "ok": True,
            "accounts": len(accounts),
            "usable": sum(1 for a in accounts if not a.disabled and a.refresh_token),
        }

    @app.post("/api/admin/login")
    async def admin_login(request: Request, response: Response) -> dict[str, Any]:
        body = await request.json()
        given = str(body.get("password") or "")
        if not settings.admin_password or not secrets.compare_digest(given, settings.admin_password):
            raise HTTPException(401, {"message": "bad password", "type": "auth_error"})
        token = secrets.token_urlsafe(24)
        admin_sessions[token] = time.time() + 8 * 3600
        response.set_cookie(SESSION_COOKIE, token, httponly=True, samesite="strict",
                            max_age=8 * 3600)
        return {"status": "authenticated", "must_change_password": False}

    # ── 授权 ─────────────────────────────────────────────
    @app.get("/api/auth/start", dependencies=[Depends(require_admin)])
    def auth_start(forceLogin: int = 0) -> dict[str, str]:
        state, challenge = sessions.create()
        return {"state": state, "url": authorize_url(state, challenge, bool(forceLogin))}

    @app.get("/api/auth/callback", dependencies=[Depends(require_admin)])
    async def auth_callback(request: Request) -> dict[str, Any]:
        params = dict(request.query_params)
        state = params.get("state", "")
        try:
            verifier = sessions.take(state)
            code = parse_callback(params.get("code") or request.url.query)["code"]
            payload = await exchange_code(code, verifier)
        except AuthError as e:
            raise HTTPException(400, {"message": str(e), "type": "auth_error"})
        acc = store.upsert(account_from_tokens(payload))
        return {"status": "ok", "account": acc.public()}

    # ── 账号 ─────────────────────────────────────────────
    @app.get("/api/accounts", dependencies=[Depends(require_admin)])
    def list_accounts() -> dict[str, Any]:
        return {"accounts": [a.public() for a in store.all()]}

    @app.delete("/api/accounts/{account_id}", dependencies=[Depends(require_admin)])
    def delete_account(account_id: str) -> dict[str, Any]:
        if not store.remove(account_id):
            raise HTTPException(404, {"message": "account not found", "type": "not_found"})
        return {"ok": True}

    # ── OpenAI 兼容 ──────────────────────────────────────
    @app.get("/v1/models", dependencies=[Depends(require_api_key)])
    def list_models() -> dict[str, Any]:
        now = int(time.time())
        return {"object": "list", "data": [
            {"id": m, "object": "model", "created": now, "owned_by": "m365-copilot"}
            for m in sorted(BASE_TONES)
        ]}

    @app.post("/v1/chat/completions", dependencies=[Depends(require_api_key)])
    async def chat_completions(request: Request):
        body = await request.json()
        model = str(body.get("model") or DEFAULT_MODEL)
        if not known_model(model):
            raise HTTPException(400, {
                "message": f"unknown model {model!r}; see GET /v1/models",
                "type": "invalid_request_error"})
        messages = body.get("messages")
        if not isinstance(messages, list) or not messages:
            raise HTTPException(400, {"message": "messages is required",
                                      "type": "invalid_request_error"})
        # tools/function-calling 无法转发给 ChatHub —— 明确拒绝, 不假装支持
        if body.get("tools") or body.get("functions"):
            raise HTTPException(400, {
                "message": "tools/function calling is not supported by this upstream",
                "type": "invalid_request_error"})

        tone = resolve_tone(model, body.get("reasoning_effort"))
        prompt = _flatten(messages)
        if not prompt.strip():
            raise HTTPException(400, {"message": "empty prompt",
                                      "type": "invalid_request_error"})

        try:
            acc = await store.usable(body.get("m365_account"))
        except AuthError as e:
            raise HTTPException(503, {"message": str(e), "type": "no_account"})

        req = ChatRequest(text=prompt, tone=tone,
                          conversation_id=str(body.get("m365_conversation_id") or ""))
        timeouts = {"overall_timeout": settings.chat_timeout_seconds,
                    "read_timeout": settings.chat_read_timeout_seconds}

        if body.get("stream"):
            return StreamingResponse(
                _sse(acc, req, model, store, timeouts),
                media_type="text/event-stream",
                headers={"Cache-Control": "no-cache", "X-Accel-Buffering": "no"},
            )
        return await _complete(acc, req, model, store, timeouts)

    return app


def _flatten(messages: list[Any]) -> str:
    """把 OpenAI messages 压成单条 prompt。ChatHub 只吃一段文本。"""
    parts = []
    for m in messages:
        if not isinstance(m, dict):
            continue
        role = str(m.get("role") or "user")
        content = m.get("content")
        if isinstance(content, list):  # 多模态数组, 只取 text 片段
            content = "".join(
                str(c.get("text") or "") for c in content
                if isinstance(c, dict) and c.get("type") == "text")
        text = str(content or "").strip()
        if not text:
            continue
        parts.append(text if role == "user" else f"[{role}]\n{text}")
    return "\n\n".join(parts)


def _chunk(cid: str, model: str, delta: dict[str, Any], finish: str | None = None) -> str:
    return "data: " + json.dumps({
        "id": cid, "object": "chat.completion.chunk", "created": int(time.time()),
        "model": model,
        "choices": [{"index": 0, "delta": delta, "finish_reason": finish}],
    }, ensure_ascii=False) + "\n\n"


async def _sse(acc: Account, req: ChatRequest, model: str,
               store: AccountStore, timeouts: dict[str, float]):
    cid = "chatcmpl-" + uuid.uuid4().hex[:24]
    produced = 0
    yield _chunk(cid, model, {"role": "assistant", "content": ""})
    try:
        async for kind, delta in stream_chat(acc, req, **timeouts):
            produced += len(delta)
            key = "reasoning_content" if kind == "reasoning" else "content"
            yield _chunk(cid, model, {key: delta})
    except ChatHubError as e:
        # 上游停滞/超时: 已经吐了一部分内容。必须发 finish_reason="length",
        # 否则客户端看到的是「输出一半然后正常结束」,分不清是不是被截断。
        if produced:
            yield _chunk(cid, model, {}, "length")
            yield ("data: " + json.dumps(
                {"m365": {"truncated": True, "produced_chars": produced, "reason": str(e)}},
                ensure_ascii=False) + "\n\n")
        else:
            store.mark(acc.id, error=str(e))
            yield ("data: " + json.dumps(
                {"error": {"message": str(e), "type": "upstream_error"}},
                ensure_ascii=False) + "\n\n")
        yield "data: [DONE]\n\n"
        return
    yield _chunk(cid, model, {}, "stop")
    yield "data: [DONE]\n\n"


async def _complete(acc: Account, req: ChatRequest, model: str,
                    store: AccountStore, timeouts: dict[str, float]) -> JSONResponse:
    answer: list[str] = []
    reasoning: list[str] = []
    truncated = False
    error = ""
    try:
        async for kind, delta in stream_chat(acc, req, **timeouts):
            (reasoning if kind == "reasoning" else answer).append(delta)
    except ChatHubError as e:
        error = str(e)
        if answer or reasoning:
            truncated = True
        else:
            store.mark(acc.id, error=error)
            return JSONResponse({"error": {"message": error, "type": "upstream_error"}},
                                status_code=502)

    text = "".join(answer)
    message: dict[str, Any] = {"role": "assistant", "content": text}
    if reasoning:
        message["reasoning_content"] = "".join(reasoning)
    payload = {
        "id": "chatcmpl-" + uuid.uuid4().hex[:24],
        "object": "chat.completion", "created": int(time.time()), "model": model,
        "choices": [{"index": 0, "message": message,
                     "finish_reason": "length" if truncated else "stop"}],
        "usage": {"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0},
    }
    if truncated:
        payload["m365"] = {"truncated": True, "reason": error}
    return JSONResponse(payload)
