# -*- coding: utf-8 -*-
"""ChatHub WebSocket 客户端。产出 (kind, text) 增量事件。

kind: "reasoning" | "answer"
"""
from __future__ import annotations

import asyncio
import time
import uuid
from dataclasses import dataclass
from typing import AsyncIterator
from urllib.parse import urlencode

from .auth import Account
from .protocol import (
    DEFAULT_VARIANTS, WS_BASE_URL, WS_ORIGIN, WS_USER_AGENT, FrameDecoder, FrameType,
    KEY_TYPE, encode_frame, frame_answer, frame_reasoning, handshake_payload,
    is_handshake_response, ping_payload, prefix_delta,
)


class ChatHubError(RuntimeError):
    pass


@dataclass
class ChatRequest:
    text: str
    tone: str
    conversation_id: str = ""
    started: bool = True


def build_ws_url(acc: Account, session_id: str, conversation_id: str, request_id: str) -> str:
    if not (acc.access_token and acc.oid and acc.tid):
        raise ChatHubError("账号缺少 access_token / oid / tid")
    params = (
        ("chatsessionid", request_id),
        ("clientrequestid", request_id),
        ("X-SessionId", session_id),
        ("ConversationId", conversation_id),
        ("access_token", acc.access_token),
        ("variants", DEFAULT_VARIANTS),
        ("source", '"officeweb"'),
        ("product", "Office"),
        ("agentHost", "Bizchat.FullScreen"),
        ("licenseType", "Starter"),
        ("scenario", "OfficeWebIncludedCopilot"),
        ("agent", "web"),
    )
    return f"{WS_BASE_URL}/{acc.oid}@{acc.tid}?{urlencode(params)}"


def chat_payload(req: ChatRequest, session_id: str, conversation_id: str, request_id: str) -> str:
    chat = {
        "arguments": [{
            "source": "officeweb",
            "clientCorrelationId": str(uuid.uuid4()),
            "sessionId": session_id,
            "optionsSets": [],
            "options": {},
            "allowedMessageTypes": [
                "Chat", "Suggestion", "Disengaged", "Progress",
                "EndOfRequest", "InternalLoaderMessage",
            ],
            "sliceIds": [],
            "threadLevelGptId": {},
            "conversationId": conversation_id,
            "traceId": str(uuid.uuid4()),
            "isStartOfSession": req.started,
            "productThreadType": "Office",
            "clientInfo": {"clientPlatform": "mcmcopilot-web", "clientAppName": "Office"},
            "tone": req.tone,
            "streamingMode": "ConciseWithPadding",
            "message": {
                "author": "user",
                "inputMethod": "Keyboard",
                "text": req.text,
                "requestId": request_id,
                "locationInfo": {"timeZoneOffset": 8, "timeZone": "Asia/Shanghai"},
                "locale": "en-US",
                "messageType": "Chat",
                "experienceType": "Default",
            },
            "plugins": [{"Id": "BingWebSearch", "Source": "BuiltIn"}],
        }],
        "invocationId": request_id,
        "target": "chat",
        "type": int(FrameType.STREAM_INVOCATION),
    }
    metrics = {
        "arguments": [{"Timestamps": {
            "ConnectionStart": "", "UserInputStart": "",
            "ConnectionEstablished": "", "UserInputSubmit": "",
        }}],
        "target": "Metrics",
        "type": int(FrameType.INVOCATION),
    }
    return encode_frame(chat) + encode_frame(metrics)


async def stream_chat(
    acc: Account,
    req: ChatRequest,
    *,
    overall_timeout: float = 900.0,
    read_timeout: float = 300.0,
) -> AsyncIterator[tuple[str, str]]:
    """连 ChatHub 跑一轮对话,逐个 yield (kind, delta)。

    两级超时:
      overall_timeout  总预算
      read_timeout     停滞检测 —— 上游会在 ~210-225s 后静默停发帧且永不发完成帧,
                       这是上游限制, 网关只能靠停滞检测收尾。
    """
    try:
        import websockets
    except ImportError as exc:
        raise ChatHubError("需要 websockets: pip install 'websockets>=14,<18'") from exc

    session_id = str(uuid.uuid4())
    conversation_id = req.conversation_id or str(uuid.uuid4())
    request_id = str(uuid.uuid4())
    url = build_ws_url(acc, session_id, conversation_id, request_id)
    headers = {
        "Origin": WS_ORIGIN,
        "Referer": WS_ORIGIN + "/",
        "User-Agent": WS_USER_AGENT,
    }

    deadline = time.monotonic() + overall_timeout
    decoder = FrameDecoder()
    seen_answer = ""
    seen_reasoning = ""  # 最近一张 CoT 卡的完整文本(用于前缀增量)
    handshook = False

    try:
        ws = await websockets.connect(
            url, additional_headers=headers,
            open_timeout=15, close_timeout=5, max_size=32 * 1024 * 1024,
        )
    except Exception as exc:
        raise ChatHubError(f"ChatHub 连接失败: {type(exc).__name__}: {exc}") from exc

    async with ws:
        await ws.send(handshake_payload())
        while True:
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                raise ChatHubError(f"总超时 {overall_timeout:.0f}s")
            try:
                raw = await asyncio.wait_for(ws.recv(), timeout=min(read_timeout, remaining))
            except asyncio.TimeoutError:
                # 停滞: 上游不再发帧。已产出内容的调用方应按「截断」处理。
                raise ChatHubError(f"上游停滞超过 {read_timeout:.0f}s(未收到完成帧)")

            for obj in decoder.feed(raw):
                if not handshook and is_handshake_response(obj):
                    if obj.get("error"):
                        raise ChatHubError(f"握手失败: {obj['error']}")
                    handshook = True
                    await ws.send(chat_payload(req, session_id, conversation_id, request_id))
                    continue

                ftype = obj.get(KEY_TYPE)
                if ftype == int(FrameType.PING):
                    await ws.send(ping_payload())
                    continue

                if ftype in (int(FrameType.STREAM_ITEM), int(FrameType.COMPLETION),
                             int(FrameType.INVOCATION)):
                    # 逐帧扫思考 —— 四种挂载形态都要扫, 否则会漏掉大部分 CoT
                    for text in frame_reasoning(obj):
                        delta = prefix_delta(seen_reasoning, text)
                        if delta:
                            seen_reasoning = text
                            yield "reasoning", delta
                    answer = frame_answer(obj)
                    if answer:
                        delta = prefix_delta(seen_answer, answer)
                        if delta:
                            seen_answer = answer
                            yield "answer", delta

                if ftype == int(FrameType.COMPLETION):
                    err = obj.get("error")
                    if err:
                        raise ChatHubError(f"上游错误: {err}")
                    return
                if ftype == int(FrameType.CLOSE):
                    err = obj.get("error")
                    if err:
                        raise ChatHubError(f"上游关闭连接: {err}")
                    return
