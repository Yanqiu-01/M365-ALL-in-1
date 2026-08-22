# -*- coding: utf-8 -*-
"""M365 Copilot → OpenAI 兼容网关(精简实现)。"""
from .config import Settings, load_settings

__all__ = ["Settings", "load_settings", "create_app"]


def create_app(settings: Settings | None = None):
    from .app import create_app as _create
    return _create(settings or load_settings())
