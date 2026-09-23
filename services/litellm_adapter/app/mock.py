from __future__ import annotations

import asyncio
import json
import re
from collections.abc import AsyncIterator
from pathlib import Path
from typing import Any

from .contracts import ChatRequest

SCRIPT_RE = re.compile(r"__script:\s*([a-z0-9-]+)")
DELAY_RE = re.compile(r"__delay:\s*(\d+)")


def _user_text(request: ChatRequest) -> str:
    text = ""
    for message in request.messages:
        if message.role == "user":
            text = "".join(part.text for part in message.parts if part.type == "text")
    return text


def _fixture_dir() -> Path:
    packaged = Path("/app/fixtures")
    if packaged.exists():
        return packaged
    return Path(__file__).resolve().parents[3] / "pkg/contracts/testdata/chat"


def _load_fixture(name: str) -> list[dict[str, Any]]:
    if not name or "/" in name or "." in name:
        raise ValueError("invalid mock script")
    path = _fixture_dir() / f"{name}.json"
    try:
        return json.loads(path.read_text(encoding="utf-8"))["stream"]
    except (OSError, KeyError, json.JSONDecodeError) as error:
        raise ValueError("unknown mock script") from error


def _latest_tool_result(request: ChatRequest) -> dict[str, Any] | None:
    for message in reversed(request.messages):
        if message.role != "tool":
            continue
        for part in message.parts:
            if part.type != "text":
                continue
            try:
                value = json.loads(part.text)
                return value if isinstance(value, dict) else None
            except ValueError:
                continue
    return None


def _tool_call(call_id: str, name: str, arguments: Any) -> list[dict[str, Any]]:
    return [
        {
            "type": "tool_call_delta",
            "provider": "mock",
            "model": "mock-editor",
            "tool_call": {
                "index": 0,
                "tool_call_id": call_id,
                "name": name,
                "arguments_delta": "",
            },
        },
        {
            "type": "tool_call_delta",
            "tool_call": {
                "index": 0,
                "tool_call_id": "",
                "name": "",
                "arguments_delta": json.dumps(arguments, separators=(",", ":")),
            },
        },
        {
            "type": "usage",
            "provider": "mock",
            "model": "mock-editor",
            "usage": {"prompt_tokens": 32, "completion_tokens": 16, "total_tokens": 48},
        },
        {"type": "done", "finish_reason": "tool_calls"},
    ]


def _editor_events(request: ChatRequest) -> list[dict[str, Any]]:
    result = _latest_tool_result(request)
    if result is None:
        return _tool_call(
            "mock-read-editor", "read_current_editor", {"selectionOnly": True}
        )
    selection = result.get("selection") if isinstance(result.get("selection"), dict) else {}
    chapter_id = result.get("chapter_id")
    original = selection.get("text")
    if not chapter_id or not original:
        return [
            {
                "type": "text_delta",
                "provider": "mock",
                "model": "mock-editor",
                "text": "Select text in the editor before asking for a revision.",
            },
            {"type": "done", "finish_reason": "stop"},
        ]
    proposal = {
        "chapterId": chapter_id,
        "baseRevision": result.get("persisted_revision"),
        "baseDocumentVersion": result.get("document_version"),
        "summary": "Tighten the selected sentence.",
        "operations": [
            {
                "type": "replace",
                "from": selection.get("from"),
                "to": selection.get("to"),
                "originalText": original,
                "replacementText": f"Tightened: {original}",
            }
        ],
    }
    return _tool_call("mock-propose-editor", "propose_editor_edit", proposal)


async def mock_events(request: ChatRequest) -> AsyncIterator[dict[str, Any]]:
    text = _user_text(request)
    script_match = SCRIPT_RE.search(text)
    delay_match = DELAY_RE.search(text)
    script = script_match.group(1) if script_match else "text-only"
    delay = min(int(delay_match.group(1)), 5000) / 1000 if delay_match else 0

    if script == "hang":
        await asyncio.Event().wait()
        return
    if script == "editor-rewrite":
        events = _editor_events(request)
    else:
        if script == "tool-then-answer":
            script = (
                "text-only"
                if any(message.role == "tool" for message in request.messages)
                else "single-tool-round"
            )
        events = _load_fixture(script)

    for event in events:
        if delay:
            await asyncio.sleep(delay)
        yield event
