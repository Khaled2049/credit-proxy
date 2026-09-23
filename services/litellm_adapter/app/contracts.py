from __future__ import annotations

from typing import Any, Literal

from pydantic import BaseModel, Field


class Usage(BaseModel):
    prompt_tokens: int = 0
    completion_tokens: int = 0
    total_tokens: int = 0


class GenerateRequest(BaseModel):
    user_id: str = ""
    prompt: str
    max_output_tokens: int = 256
    temperature: float = 0.7
    force_mock: bool = False
    byok_provider: str = ""
    byok_api_key: str = ""
    byok_model: str = ""


class GenerateResponse(BaseModel):
    output: str
    model: str
    usage: Usage


class ChatPart(BaseModel):
    type: Literal["text", "tool_call"]
    text: str = ""
    tool_call_id: str = ""
    name: str = ""
    arguments: Any = None
    provider_meta: Any = None


class ChatMessage(BaseModel):
    role: Literal["system", "user", "assistant", "tool"]
    parts: list[ChatPart]
    tool_call_id: str = ""


class ToolSchema(BaseModel):
    name: str
    description: str = ""
    parameters: Any


class ToolChoice(BaseModel):
    mode: Literal["auto", "none", "required"]
    name: str = ""


class ChatRequest(BaseModel):
    version: Literal[1]
    user_id: str
    messages: list[ChatMessage] = Field(min_length=1)
    tools: list[ToolSchema] = Field(default_factory=list)
    tool_choice: ToolChoice | None = None
    max_output_tokens: int = Field(gt=0)
    temperature: float = 0.7
    stream: bool = False
    force_mock: bool = False
    byok_provider: str = ""
    byok_api_key: str = ""
    byok_model: str = ""


class ValidateRequest(BaseModel):
    provider: str
    api_key: str = Field(min_length=1)
    model: str | None = None
