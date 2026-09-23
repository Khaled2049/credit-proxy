from __future__ import annotations

import json
import logging
import os
from collections.abc import AsyncIterator
from typing import Any

import litellm

from .catalog import litellm_model
from .contracts import ChatMessage, ChatRequest, ToolChoice, ToolSchema, Usage

logger = logging.getLogger(__name__)

# Provider responses can contain prompts or manuscript text. Keep LiteLLM quiet
# and ensure callers receive only the adapter's normalized errors.
litellm.suppress_debug_info = True
litellm.set_verbose = False


class AdapterError(Exception):
    def __init__(self, status_code: int, code: str, retryable: bool = False):
        self.status_code = status_code
        self.code = code
        self.retryable = retryable
        super().__init__(code)


def classify_error(error: Exception) -> AdapterError:
    if isinstance(error, AdapterError):
        return error
    if isinstance(error, litellm.AuthenticationError):
        return AdapterError(401, "provider_auth", False)
    if isinstance(error, litellm.RateLimitError):
        return AdapterError(429, "rate_limited", True)
    if isinstance(error, litellm.Timeout):
        return AdapterError(504, "provider_timeout", True)
    if isinstance(error, litellm.NotFoundError):
        return AdapterError(404, "provider_not_found", False)
    if isinstance(error, litellm.BadRequestError):
        return AdapterError(400, "invalid_request", False)
    if isinstance(error, litellm.APIConnectionError):
        return AdapterError(503, "provider_unavailable", True)
    return AdapterError(502, "provider_error", True)


def _platform_identity() -> tuple[str, str, str, str]:
    configured = os.getenv("PLATFORM_MODEL", "").strip()
    if configured and "/" in configured:
        provider, model = configured.split("/", 1)
    else:
        provider = os.getenv("LLM_PROVIDER", "gemini").strip().lower()
        if provider == "claude":
            provider = "anthropic"
        model_env = {
            "gemini": "GEMINI_MODEL",
            "anthropic": "ANTHROPIC_MODEL",
            "openai": "OPENAI_MODEL",
        }.get(provider, "")
        model = os.getenv(model_env, "").strip() if model_env else ""
    try:
        canonical, selected, routed = litellm_model(provider, model or None)
    except ValueError:
        raise AdapterError(503, "provider_unavailable", True) from None
    api_key = os.getenv(
        {
            "gemini": "GEMINI_API_KEY",
            "anthropic": "ANTHROPIC_API_KEY",
            "openai": "OPENAI_API_KEY",
        }[canonical],
        "",
    ).strip()
    if not api_key:
        raise AdapterError(503, "provider_unavailable", True)
    return canonical, selected, routed, api_key


def request_identity(request: ChatRequest) -> tuple[str, str, str, str]:
    if request.byok_provider or request.byok_api_key:
        if not request.byok_provider or not request.byok_api_key:
            raise AdapterError(400, "invalid_request", False)
        try:
            canonical, selected, routed = litellm_model(
                request.byok_provider, request.byok_model or None
            )
        except ValueError:
            raise AdapterError(400, "invalid_request", False) from None
        return canonical, selected, routed, request.byok_api_key
    return _platform_identity()


def _message(message: ChatMessage) -> dict[str, Any]:
    text = "".join(part.text for part in message.parts if part.type == "text")
    if message.role == "tool":
        return {
            "role": "tool",
            "tool_call_id": message.tool_call_id,
            "content": text,
        }
    converted: dict[str, Any] = {"role": message.role, "content": text or None}
    calls = []
    for part in message.parts:
        if part.type != "tool_call":
            continue
        arguments = part.arguments
        if not isinstance(arguments, str):
            arguments = json.dumps(arguments if arguments is not None else {})
        call: dict[str, Any] = {
            "id": part.tool_call_id,
            "type": "function",
            "function": {"name": part.name, "arguments": arguments},
        }
        if part.provider_meta is not None:
            # LiteLLM preserves provider-specific fields while translating the
            # common OpenAI-shaped tool call for Gemini/Anthropic.
            call["provider_specific_fields"] = part.provider_meta
        calls.append(call)
    if calls:
        converted["tool_calls"] = calls
    return converted


def _tools(tools: list[ToolSchema]) -> list[dict[str, Any]]:
    return [
        {
            "type": "function",
            "function": {
                "name": tool.name,
                "description": tool.description,
                "parameters": tool.parameters,
            },
        }
        for tool in tools
    ]


def _tool_choice(choice: ToolChoice | None) -> Any:
    if choice is None or choice.mode == "auto":
        return "auto"
    if choice.mode == "none":
        return "none"
    if choice.name:
        return {"type": "function", "function": {"name": choice.name}}
    return "required"


def _as_dict(value: Any) -> dict[str, Any]:
    if value is None:
        return {}
    if isinstance(value, dict):
        return value
    dump = getattr(value, "model_dump", None)
    if callable(dump):
        return dump(exclude_none=True)
    return {}


def _usage(raw: Any) -> Usage:
    data = _as_dict(raw)
    prompt = int(data.get("prompt_tokens") or 0)
    completion = int(data.get("completion_tokens") or 0)
    return Usage(
        prompt_tokens=prompt,
        completion_tokens=completion,
        total_tokens=int(data.get("total_tokens") or prompt + completion),
    )


def _provider_meta(call: dict[str, Any]) -> Any:
    if call.get("provider_specific_fields") is not None:
        return call["provider_specific_fields"]
    if call.get("provider_meta") is not None:
        return call["provider_meta"]
    signature = call.get("thoughtSignature") or call.get("thought_signature")
    return {"thoughtSignature": signature} if signature else None


async def completion(request: ChatRequest, *, stream: bool) -> Any:
    _, _, routed_model, api_key = request_identity(request)
    params: dict[str, Any] = {
        "model": routed_model,
        "api_key": api_key,
        "messages": [_message(message) for message in request.messages],
        "max_tokens": request.max_output_tokens,
        "stream": stream,
        "timeout": 300,
        "num_retries": 0,
        "drop_params": True,
    }
    if request.temperature is not None:
        params["temperature"] = request.temperature
    if request.tools:
        params["tools"] = _tools(request.tools)
        params["tool_choice"] = _tool_choice(request.tool_choice)
    if stream:
        params["stream_options"] = {"include_usage": True}
    try:
        return await litellm.acompletion(**params)
    except Exception as error:
        raise classify_error(error) from None


def estimated_usage(prompt: str, output: str) -> Usage:
    # Mirrors creditProxy's conservative four-characters-per-token fallback.
    prompt_tokens = max(1, (len(prompt) + 3) // 4)
    completion_tokens = max(1, (len(output) + 3) // 4)
    return Usage(
        prompt_tokens=prompt_tokens,
        completion_tokens=completion_tokens,
        total_tokens=prompt_tokens + completion_tokens,
    )


async def stream_events(
    request: ChatRequest,
    chunks: AsyncIterator[Any] | None = None,
) -> AsyncIterator[dict[str, Any]]:
    canonical = "unknown"
    selected = "unknown"
    try:
        canonical, selected, _, _ = request_identity(request)
        response = chunks if chunks is not None else await completion(request, stream=True)
        usage = Usage()
        finish_reason = "stop"
        async for raw_chunk in response:
            chunk = _as_dict(raw_chunk)
            if chunk.get("usage"):
                usage = _usage(chunk["usage"])
            choices = chunk.get("choices") or []
            if not choices:
                continue
            choice = _as_dict(choices[0])
            delta = _as_dict(choice.get("delta"))
            content = delta.get("content")
            if isinstance(content, str) and content:
                yield {
                    "type": "text_delta",
                    "provider": canonical,
                    "model": selected,
                    "text": content,
                }
            for raw_call in delta.get("tool_calls") or []:
                call = _as_dict(raw_call)
                function = _as_dict(call.get("function"))
                event: dict[str, Any] = {
                    "type": "tool_call_delta",
                    "provider": canonical,
                    "model": selected,
                    "tool_call": {
                        "index": int(call.get("index") or 0),
                        "tool_call_id": call.get("id") or "",
                        "name": function.get("name") or "",
                        "arguments_delta": function.get("arguments") or "",
                    },
                }
                meta = _provider_meta(call)
                if meta is not None:
                    event["tool_call"]["provider_meta"] = meta
                yield event
            if choice.get("finish_reason"):
                finish_reason = str(choice["finish_reason"])
        # No usage frame is safer than a zero-usage frame: the Go gateway then
        # commits its conservative reservation ceiling instead of billing zero.
        if usage.total_tokens > 0:
            yield {
                "type": "usage",
                "provider": canonical,
                "model": selected,
                "usage": usage.model_dump(),
            }
        normalized = {
            "tool_calls": "tool_calls",
            "length": "length",
            "max_tokens": "length",
        }.get(finish_reason, "stop")
        yield {"type": "done", "finish_reason": normalized}
    except Exception as error:
        classified = classify_error(error)
        yield {
            "type": "error",
            "provider": canonical,
            "model": selected,
            "finish_reason": "error",
            "error": {
                "code": classified.code,
                "message": "provider request failed",
                "retryable": classified.retryable,
            },
        }
