---
name: fastrouter-responses
description: OpenAI Responses API through FastRouter (POST /v1/responses). Use for the Codex CLI, OpenAI Agents SDK or any client that speaks Responses, including routing to Codex sign-in accounts.
---

# FastRouter: Responses

Base URL `${FASTROUTER_URL}/v1`, key `Authorization: Bearer ${FASTROUTER_KEY}`.

## Request

```bash
curl $FASTROUTER_URL/v1/responses \
  -H "Authorization: Bearer $FASTROUTER_KEY" -H "Content-Type: application/json" \
  -d '{"model": "cx/gpt-5.5", "input": "Summarise the README in 3 bullets", "stream": true}'
```

- `input` may be a string or a list of message / function-call items; `tools`, `reasoning` and `instructions` pass through.
- Codex sign-in accounts (`cx/...`) and other Responses upstreams receive the request as is; Chat, Anthropic and Gemini upstreams are translated.
- Streaming returns Responses SSE events (`response.created`, `response.output_text.delta`, ... `response.completed`).

## Codex CLI

```toml
# ~/.codex/config.toml
model = "cx/gpt-5.5"
model_provider = "fastrouter"

[model_providers.fastrouter]
name = "FastRouter"
base_url = "http://localhost:20128/v1"
env_key = "FASTROUTER_KEY"
wire_api = "responses"
```
