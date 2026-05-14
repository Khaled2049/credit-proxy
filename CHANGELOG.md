# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.2.0] - 2026-04-30

### Added
- Multi-provider LLM support: Gemini, OpenAI, Anthropic, Ollama, mock
- `LLM_PROVIDER` env var selects server-wide provider at startup
- Per-request BYOK override (`byok_provider`, `byok_api_key`, `byok_model`) — skips platform credits, still logs to ledger as `byok_generate`
- Provider abstraction (`provider.go`) with per-provider files (`gemini.go`, `openai.go`, `anthropic.go`, `ollama.go`, `mock.go`)
- `OPENAI_BASE_URL` / `ANTHROPIC_BASE_URL` overrides for compatible APIs
- `OLLAMA_BASE_URL` and `OLLAMA_MODEL` config

## [0.1.0] - 2026-04-26

### Added
- Four-service distributed system: gateway, usage, llmproxy, ledger
- Credit reservation lifecycle with atomic Redis Lua scripts (reserve → commit/release)
- Gemini API integration with mock mode (`LLM_MOCK_MODE=true`)
- Append-only Postgres ledger with idempotency via `ON CONFLICT`
- Reservation TTL for automatic cleanup of stuck reservations
- `/healthz` endpoints on all services exposing version and commit
- OpenAPI 3.0 specs for all four services (`docs/openapi/`)
- BDD smoke test suite using Godog / Cucumber (`tests/smoke/`)
- Semantic versioning with `pkg/version` and build-time ldflags injection
- `make tag`, `make build-images`, `make release` targets
