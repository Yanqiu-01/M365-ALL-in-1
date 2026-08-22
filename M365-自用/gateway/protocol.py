# -*- coding: utf-8 -*-
"""ChatHub 协议常量与帧解析。

常量照抄自可用实现,不要凭记忆改动:
  * OAuth: login.microsoftonline.com/common, nativeclient 重定向
  * 传输: wss://substrate.office.com/m365Copilot/Chathub, SignalR JSON
  * ChatHub 收的是 *tone* 不是模型名, tone 决定上游是否发 chain-of-thought
"""
from __future__ import annotations

import json
from enum import IntEnum
from typing import Any, Final, Mapping

# ── OAuth (PKCE, public client) ──────────────────────────
CLIENT_ID: Final[str] = "c0ab8ce9-e9a0-42e7-b064-33d422df41f1"
AUTHORITY: Final[str] = "https://login.microsoftonline.com/common"
REDIRECT_URI: Final[str] = f"{AUTHORITY}/oauth2/nativeclient"
AUTHORIZE_URL: Final[str] = f"{AUTHORITY}/oauth2/v2.0/authorize"
TOKEN_URL: Final[str] = f"{AUTHORITY}/oauth2/v2.0/token"
SCOPES: Final[tuple[str, ...]] = (
    "https://substrate.office.com/sydney/M365Chat.Read",
    "https://substrate.office.com/sydney/sydney.readwrite",
    "offline_access",
    "openid",
    "profile",
)
# 换取 token 时必须发浏览器 UA, 且**不能**发 Origin ——
# 否则 AAD 对这个 public client 回 AADSTS9002326 (SPA-only cross-origin redemption)。
TOKEN_USER_AGENT: Final[str] = (
    "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 "
    "(KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"
)

# ── ChatHub 传输 ─────────────────────────────────────────
WS_BASE_URL: Final[str] = "wss://substrate.office.com/m365Copilot/Chathub"
WS_ORIGIN: Final[str] = "https://m365.cloud.microsoft"
WS_USER_AGENT: Final[str] = (
    "Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:148.0) Gecko/20100101 Firefox/148.0"
)
DEFAULT_VARIANTS: Final[str] = (
    "EnableMcpServerWidgets,feature.EnableMcpServerWidgets,"
    "feature.IsStreamingModeInChatRequestEnabled,"
    "IncludeSourceAttributionsConcise,SkipPublishEmptyMessage,"
    "feature.EnableReferencesListCompleteSignal,"
    "Agt_bizchat_enableGpt5ForHelix"
)

RECORD_SEPARATOR: Final[str] = "\x1e"


class FrameType(IntEnum):
    """SignalR 消息类型。出站 chat 用 4 (StreamInvocation)。"""
    INVOCATION = 1
    STREAM_ITEM = 2
    COMPLETION = 3
    STREAM_INVOCATION = 4
    CANCEL_INVOCATION = 5
    PING = 6
    CLOSE = 7


KEY_TYPE: Final[str] = "type"
KEY_ARGUMENTS: Final[str] = "arguments"
KEY_MESSAGES: Final[str] = "messages"
KEY_ITEM: Final[str] = "item"
KEY_TEXT: Final[str] = "text"
KEY_AUTHOR: Final[str] = "author"
KEY_MESSAGE_TYPE: Final[str] = "messageType"
KEY_CONTENT_TYPE: Final[str] = "contentType"
KEY_CONTENT_ORIGIN: Final[str] = "contentOrigin"
KEY_ADD_TO_COT: Final[str] = "addToChainOfThought"
KEY_HIDDEN_TEXT: Final[str] = "hiddenText"

AUTHOR_BOT: Final[str] = "bot"
CONTENT_ORIGIN_COT: Final[str] = "ChainOfThoughtSummary"
MESSAGE_TYPE_PROGRESS: Final[str] = "Progress"
PROGRESS_CONTENT_TYPES: Final[frozenset[str]] = frozenset({"SearchResults", "Code", "ToolCall"})


# ── tone 路由 ────────────────────────────────────────────
DEFAULT_TONE: Final[str] = "magic"
DEFAULT_MODEL: Final[str] = "m365-copilot"

BASE_TONES: Final[dict[str, str]] = {
    DEFAULT_MODEL: DEFAULT_TONE,
    "gpt-5.2": "Gpt_5_2_Chat",
    "gpt-5.2-reasoning": "Gpt_5_2_Reasoning",
    "gpt-5.3": "Gpt_5_3_Chat",
    "gpt-5.3-reasoning": "Gpt_5_3_Reasoning",
    "gpt-5.4": "Gpt_5_4_Chat",
    "gpt-5.4-reasoning": "Gpt_5_4_Reasoning",
    "gpt-5.5": "Gpt_5_5_Chat",
    "gpt-5.5-reasoning": "Gpt_5_5_Reasoning",
    "gpt-5.6-reasoning": "Gpt_5_6_Reasoning",
    "claude-sonnet": "Claude_Sonnet",
    "claude-sonnet-reasoning": "Claude_Sonnet_Reasoning",
    # 上游把这两个也映射到 chat tone, 名字里带 quick/think 但并非 reasoning 模型
    "gpt-5.4-quick": "Gpt_5_4_Chat",
    "gpt-5.3-think-deeper": "Gpt_5_3_Chat",
}

_REASONING_UPGRADE: Final[dict[str, str]] = {
    "Gpt_5_2_Chat": "Gpt_5_2_Reasoning",
    "Gpt_5_3_Chat": "Gpt_5_3_Reasoning",
    "Gpt_5_4_Chat": "Gpt_5_4_Reasoning",
    "Gpt_5_5_Chat": "Gpt_5_5_Reasoning",
    "Claude_Sonnet": "Claude_Sonnet_Reasoning",
}
# 这几档不升级 tone
_NON_ESCALATING_EFFORTS: Final[frozenset[str]] = frozenset({"none", "minimal", "low"})


def known_model(model: str) -> bool:
    return (model or "").strip().lower() in BASE_TONES


def resolve_tone(model: str, effort: str | None = None) -> str:
    """模型名 + reasoning_effort → tone。已是 reasoning 的不降级。"""
    key = (model or "").strip().lower() or DEFAULT_MODEL
    tone = BASE_TONES.get(key, DEFAULT_TONE)
    eff = (effort or "").strip().lower()
    if eff and eff not in _NON_ESCALATING_EFFORTS:
        tone = _REASONING_UPGRADE.get(tone, tone)
    return tone


# ── 帧编解码 ─────────────────────────────────────────────
def encode_frame(obj: Mapping[str, Any]) -> str:
    return json.dumps(obj, separators=(",", ":"), ensure_ascii=False) + RECORD_SEPARATOR


def handshake_payload() -> str:
    return json.dumps({"protocol": "json", "version": 1}, separators=(",", ":")) + RECORD_SEPARATOR


def ping_payload() -> str:
    return json.dumps({KEY_TYPE: int(FrameType.PING)}, separators=(",", ":")) + RECORD_SEPARATOR


class FrameDecoder:
    """有状态的 RS 分帧器。

    不能按每个 WebSocket 消息 split —— 一帧可能跨两个消息, 那样会静默丢数据。
    """

    __slots__ = ("_buf",)

    def __init__(self) -> None:
        self._buf: str = ""

    def feed(self, chunk: str | bytes) -> list[dict[str, Any]]:
        if isinstance(chunk, bytes):
            chunk = chunk.decode("utf-8", "replace")
        self._buf += chunk
        if RECORD_SEPARATOR not in self._buf:
            return []
        parts = self._buf.split(RECORD_SEPARATOR)
        self._buf = parts.pop()  # 末尾残片留到下次
        out = []
        for p in parts:
            p = p.strip()
            if not p:
                continue
            try:
                obj = json.loads(p)
            except ValueError:
                continue
            if isinstance(obj, dict):
                out.append(obj)
        return out


def is_handshake_response(obj: Mapping[str, Any]) -> bool:
    """握手响应成功是 {}, 失败带 error;它没有 type 键,以此区分。"""
    return KEY_TYPE not in obj


def candidate_messages(frame_obj: Mapping[str, Any]) -> list[dict[str, Any]]:
    """帧里所有「消息形状」的 map,覆盖上游用的全部四种挂载位置。

    chain-of-thought 不走固定路径: 可能在 arguments[].messages、arguments[] 自身、
    type-2 帧的 item.messages、以及顶层 messages。只扫第一种会静默丢掉另外三种
    (症状: 思考内容少几千字, 或思考时长显示为 0)。
    """
    out: list[dict[str, Any]] = []

    def add_all(value: Any) -> None:
        if isinstance(value, list):
            out.extend(i for i in value if isinstance(i, dict))

    arguments = frame_obj.get(KEY_ARGUMENTS)
    if isinstance(arguments, list):
        for arg in arguments:
            if isinstance(arg, dict):
                out.append(arg)  # 有些帧把消息 map 直接放这
                add_all(arg.get(KEY_MESSAGES))
    item = frame_obj.get(KEY_ITEM)
    if isinstance(item, dict):
        add_all(item.get(KEY_MESSAGES))
    add_all(frame_obj.get(KEY_MESSAGES))
    return out


def frame_reasoning(frame_obj: Mapping[str, Any]) -> list[str]:
    """帧内的思考文本。只认显式标记,普通正文绝不会被误当作 reasoning。"""
    found: list[str] = []
    for msg in candidate_messages(frame_obj):
        if msg.get(KEY_CONTENT_ORIGIN) != CONTENT_ORIGIN_COT and msg.get(KEY_ADD_TO_COT) is not True:
            continue
        text = msg.get(KEY_TEXT)
        if not (isinstance(text, str) and text):
            text = msg.get(KEY_HIDDEN_TEXT)
        if isinstance(text, str) and text:
            found.append(text)
    return found


def frame_answer(frame_obj: Mapping[str, Any]) -> str:
    """帧内的正文快照(累积快照,不是增量)。进度/工具卡片不算正文。"""
    best = ""
    for msg in candidate_messages(frame_obj):
        if msg.get(KEY_CONTENT_ORIGIN) == CONTENT_ORIGIN_COT or msg.get(KEY_ADD_TO_COT) is True:
            continue
        mt = msg.get(KEY_MESSAGE_TYPE)
        ct = msg.get(KEY_CONTENT_TYPE)
        if mt == MESSAGE_TYPE_PROGRESS or (isinstance(ct, str) and ct in PROGRESS_CONTENT_TYPES):
            continue
        if mt not in (None, "", "Chat"):
            continue
        if msg.get(KEY_AUTHOR) != AUTHOR_BOT:
            continue
        text = msg.get(KEY_TEXT)
        if isinstance(text, str) and len(text) > len(best):
            best = text
    return best


def prefix_delta(previous: str, current: str) -> str:
    """前缀增量。上游可能发增长快照,直接比整串会把 think→thinking 拼成 thinkthinking。"""
    if not previous:
        return current
    if current.startswith(previous):
        return current[len(previous):]
    return current if current != previous else ""
