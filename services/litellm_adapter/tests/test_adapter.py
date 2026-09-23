from __future__ import annotations

import json

import pytest
from fastapi.testclient import TestClient

from app.catalog import litellm_model
from app.main import app
from app.mock import mock_events
from app.contracts import ChatRequest
from app.provider import AdapterError, estimated_usage


def request_payload(**overrides):
    payload = {
        "version": 1,
        "user_id": "user-1",
        "messages": [{"role": "user", "parts": [{"type": "text", "text": "Hello"}]}],
        "max_output_tokens": 64,
        "force_mock": True,
    }
    payload.update(overrides)
    return payload


def test_catalog_resolves_legacy_anthropic_alias():
    assert litellm_model("claude", None) == (
        "anthropic",
        "claude-haiku-4-5-20251001",
        "anthropic/claude-haiku-4-5-20251001",
    )


def test_catalog_rejects_models_outside_curated_allowlist():
    with pytest.raises(ValueError):
        litellm_model("openai", "made-up-model")


def test_catalog_endpoint_requires_internal_token(monkeypatch):
    monkeypatch.setenv("INTERNAL_SERVICE_TOKEN", "secret")
    client = TestClient(app)
    assert client.get("/v1/providers").status_code == 401
    response = client.get("/v1/providers", headers={"X-Internal-Token": "secret"})
    assert response.status_code == 200
    assert {item["id"] for item in response.json()["providers"]} == {
        "gemini",
        "anthropic",
        "openai",
    }


def test_buffered_mock_replays_shared_contract_fixture(monkeypatch):
    monkeypatch.delenv("INTERNAL_SERVICE_TOKEN", raising=False)
    response = TestClient(app).post("/v1/chat", json=request_payload(stream=False))
    assert response.status_code == 200
    body = response.json()
    assert body["provider"] == "gemini"
    assert body["usage"]["total_tokens"] > 0
    assert "lighthouse" in body["parts"][0]["text"].lower()


@pytest.mark.asyncio
async def test_scripted_tool_round_uses_existing_fixture():
    request = ChatRequest.model_validate(
        request_payload(
            messages=[
                {
                    "role": "user",
                    "parts": [{"type": "text", "text": "Please revise. __script: tool-then-answer"}],
                }
            ]
        )
    )
    events = [event async for event in mock_events(request)]
    assert any(event["type"] == "tool_call_delta" for event in events)
    assert events[-1]["finish_reason"] == "tool_calls"


def test_generate_mock_uses_nonzero_estimated_usage(monkeypatch):
    monkeypatch.setenv("LLM_PROVIDER", "mock")
    response = TestClient(app).post(
        "/v1/generate", json={"user_id": "user-1", "prompt": "Write a line"}
    )
    assert response.status_code == 200
    assert response.json()["usage"]["total_tokens"] > 0


def test_token_estimate_never_reports_zero():
    usage = estimated_usage("", "")
    assert usage.prompt_tokens == 1
    assert usage.completion_tokens == 1


@pytest.mark.asyncio
async def test_stream_preflight_maps_provider_error_before_sse(monkeypatch):
    async def fail(*_args, **_kwargs):
        raise AdapterError(401, "provider_auth")

    monkeypatch.setattr("app.main.completion", fail)
    monkeypatch.setenv("LLM_PROVIDER", "gemini")
    response = TestClient(app).post(
        "/v1/chat",
        json=request_payload(
            stream=True,
            force_mock=False,
            byok_provider="gemini",
            byok_api_key="invalid",
            byok_model="gemini-2.5-flash",
        ),
    )
    assert response.status_code == 401
    assert json.loads(response.text)["detail"] == "provider_auth"
