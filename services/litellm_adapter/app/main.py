from __future__ import annotations

import json
import os
from collections.abc import AsyncIterator
from typing import Any

from fastapi import Depends, FastAPI, Header, HTTPException
from fastapi.responses import StreamingResponse

from .catalog import catalog, litellm_model
from .contracts import ChatRequest, GenerateRequest, ValidateRequest
from .mock import mock_events
from .provider import (
    AdapterError,
    _as_dict,
    _usage,
    classify_error,
    completion,
    estimated_usage,
    request_identity,
    stream_events,
)

app = FastAPI(title="creditProxy LiteLLM adapter", version="1.0.0")

if os.getenv("LOCAL_UNMETERED", "false").lower() == "true":
    raise RuntimeError(
        "LOCAL_UNMETERED is not supported by the hosted LiteLLM adapter; "
        "disable it so provider calls remain metered"
    )


def require_internal_token(x_internal_token: str | None = Header(default=None)) -> None:
    expected = os.getenv("INTERNAL_SERVICE_TOKEN", "")
    if expected and x_internal_token != expected:
        raise HTTPException(status_code=401, detail="unauthorized")


def _mock_generate(request: GenerateRequest) -> dict[str, Any]:
    output = f"Mock response to: {request.prompt}"
    return {
        "output": output,
        "model": "mock-gemini",
        "usage": estimated_usage(request.prompt, output).model_dump(),
    }


async def _event_source(events: AsyncIterator[dict[str, Any]]) -> AsyncIterator[str]:
    async for event in events:
        yield f"data: {json.dumps(event, separators=(',', ':'))}\n\n"


async def _prepared_events(request: ChatRequest) -> AsyncIterator[dict[str, Any]]:
    """Open the provider stream and read its first frame before returning 200."""
    response = await completion(request, stream=True)
    iterator = response.__aiter__()
    try:
        first = await anext(iterator)
    except StopAsyncIteration:
        first = None
    except Exception as error:
        raise classify_error(error) from None

    async def chunks() -> AsyncIterator[Any]:
        if first is not None:
            yield first
        async for chunk in iterator:
            yield chunk

    return stream_events(request, chunks())


@app.get("/health")
async def health() -> dict[str, str]:
    return {"status": "ok", "provider": "litellm"}


@app.get("/v1/providers", dependencies=[Depends(require_internal_token)])
async def providers() -> dict[str, Any]:
    return catalog()


@app.post("/v1/providers/validate", dependencies=[Depends(require_internal_token)])
async def validate_provider(request: ValidateRequest) -> dict[str, Any]:
    try:
        canonical, selected, _ = litellm_model(request.provider, request.model)
        validation = ChatRequest(
            version=1,
            user_id="validation",
            messages=[{"role": "user", "parts": [{"type": "text", "text": "Reply OK"}]}],
            max_output_tokens=1,
            temperature=0,
            byok_provider=canonical,
            byok_api_key=request.api_key,
            byok_model=selected,
        )
        await completion(validation, stream=False)
        return {"valid": True, "provider": canonical, "model": selected}
    except Exception as error:
        classified = classify_error(error)
        return {"valid": False, "error": classified.code}


@app.post("/v1/generate", dependencies=[Depends(require_internal_token)])
async def generate(request: GenerateRequest) -> dict[str, Any]:
    if request.force_mock or os.getenv("LLM_PROVIDER", "").lower() == "mock":
        return _mock_generate(request)
    chat = ChatRequest(
        version=1,
        user_id=request.user_id or "platform",
        messages=[{"role": "user", "parts": [{"type": "text", "text": request.prompt}]}],
        max_output_tokens=request.max_output_tokens or 256,
        temperature=request.temperature,
        byok_provider=request.byok_provider,
        byok_api_key=request.byok_api_key,
        byok_model=request.byok_model,
    )
    try:
        _, selected, _, _ = request_identity(chat)
        response = await completion(chat, stream=False)
        data = _as_dict(response)
        choices = data.get("choices") or []
        message = _as_dict(_as_dict(choices[0]).get("message")) if choices else {}
        usage = _usage(data.get("usage"))
        return {
            "output": message.get("content") or "",
            "model": data.get("model") or selected,
            "usage": (
                usage
                if usage.total_tokens > 0
                else estimated_usage(request.prompt, message.get("content") or "")
            ).model_dump(),
        }
    except AdapterError as error:
        raise HTTPException(status_code=error.status_code, detail=error.code) from None


@app.post("/v1/chat", dependencies=[Depends(require_internal_token)])
async def chat(request: ChatRequest):
    mock = request.force_mock or os.getenv("LLM_PROVIDER", "").lower() == "mock"
    if request.stream:
        try:
            prepared = mock_events(request) if mock else await _prepared_events(request)
        except AdapterError as error:
            raise HTTPException(status_code=error.status_code, detail=error.code) from None
        stream = _event_source(prepared)
        return StreamingResponse(stream, media_type="text/event-stream")

    if mock:
        events = [event async for event in mock_events(request)]
    else:
        events = [event async for event in stream_events(request)]
    failed = next((event for event in events if event["type"] == "error"), None)
    if failed:
        code = failed.get("error", {}).get("code", "provider_error")
        status = {
            "provider_auth": 401,
            "rate_limited": 429,
            "provider_timeout": 504,
            "provider_not_found": 404,
            "invalid_request": 400,
            "provider_unavailable": 503,
        }.get(code, 502)
        raise HTTPException(status_code=status, detail=code)
    text = "".join(event.get("text", "") for event in events if event["type"] == "text_delta")
    usage: dict[str, Any] = next((event.get("usage", {}) for event in events if event["type"] == "usage"), {})
    done = next((event for event in events if event["type"] == "done"), {"finish_reason": "error"})
    provider = next((event.get("provider") for event in events if event.get("provider")), "unknown")
    model = next((event.get("model") for event in events if event.get("model")), "unknown")
    tool_calls: dict[int, dict[str, Any]] = {}
    for event in events:
        if event["type"] != "tool_call_delta":
            continue
        delta = event["tool_call"]
        item = tool_calls.setdefault(delta["index"], {"type": "tool_call", "tool_call_id": "", "name": "", "arguments": ""})
        item["tool_call_id"] = delta.get("tool_call_id") or item["tool_call_id"]
        item["name"] = delta.get("name") or item["name"]
        item["arguments"] += delta.get("arguments_delta") or ""
        if delta.get("provider_meta") is not None:
            item["provider_meta"] = delta["provider_meta"]
    parts: list[dict[str, Any]] = []
    if text:
        parts.append({"type": "text", "text": text})
    for index in sorted(tool_calls):
        item = tool_calls[index]
        try:
            item["arguments"] = json.loads(item["arguments"] or "{}")
        except ValueError:
            pass
        parts.append(item)
    return {"provider": provider, "model": model, "parts": parts, "usage": usage, "credits": 0, "finish_reason": done["finish_reason"]}
