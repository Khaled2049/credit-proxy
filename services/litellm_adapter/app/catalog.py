from __future__ import annotations

import json
from functools import lru_cache
from pathlib import Path
from typing import Any

PROVIDER_ALIASES = {"claude": "anthropic"}
LITELLM_PREFIXES = {
    "gemini": "gemini",
    "anthropic": "anthropic",
    "openai": "openai",
}


@lru_cache(maxsize=1)
def catalog() -> dict[str, Any]:
    path = Path(__file__).with_name("catalog.json")
    return json.loads(path.read_text(encoding="utf-8"))


def canonical_provider(provider: str) -> str:
    normalized = provider.strip().lower()
    return PROVIDER_ALIASES.get(normalized, normalized)


def provider_entry(provider: str) -> dict[str, Any]:
    canonical = canonical_provider(provider)
    for entry in catalog()["providers"]:
        if entry["id"] == canonical:
            return entry
    raise ValueError(f"unsupported provider: {provider}")


def resolve_model(provider: str, model: str | None) -> tuple[str, str]:
    entry = provider_entry(provider)
    selected = (model or "").strip() or entry["default_model"]
    if selected not in {item["id"] for item in entry["models"]}:
        raise ValueError(f"unsupported model for {entry['id']}: {selected}")
    return entry["id"], selected


def litellm_model(provider: str, model: str | None) -> tuple[str, str, str]:
    canonical, selected = resolve_model(provider, model)
    return canonical, selected, f"{LITELLM_PREFIXES[canonical]}/{selected}"
