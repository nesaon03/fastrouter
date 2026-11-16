---
name: fastrouter-messages
description: Anthropic Messages API through FastRouter (POST /v1/messages and /v1/messages/count_tokens). Use for Claude Code, the Anthropic SDK or any Claude-style client pointed at FastRouter.
---

# FastRouter: Anthropic Messages

Base URL `${FASTROUTER_URL}` (no `/v1` for Anthropic SDKs), key in `x-api-key` or `Authorization: Bearer`.

## Request

```bash
curl $FASTROUTER_URL/v1/messages \
  -H "x-api-key: $FASTROUTER_KEY" -H "anthropic-version: 2023-06-01" -H "Content-Type: application/json" \
  -d '{
    "model": "glm/glm-5",
    "max_tokens": 1024,
    "messages": [{"role": "user", "content": "Explain this stack trace"}]
  }'
```

- Any FastRouter model works, not just Claude: non-Anthropic upstreams are translated (tools, images, thinking and streaming events included).
- `stream: true` returns Anthropic SSE events (`message_start`, `content_block_delta`, ... `message_stop`).
- `anthropic-version` and `anthropic-beta` headers are forwarded to Anthropic-format upstreams.
- Token count: `POST /v1/messages/count_tokens` with the same body returns `{"input_tokens": N}`, a local estimate (no upstream call).

## Claude Code

```bash
export ANTHROPIC_BASE_URL="http://localhost:20128"
export ANTHROPIC_AUTH_TOKEN="$FASTROUTER_KEY"
export ANTHROPIC_MODEL="my-combo-team"          # any prefix/model or combo
claude
```
