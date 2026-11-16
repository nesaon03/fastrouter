---
name: fastrouter-gemini
description: Google Gemini generateContent API through FastRouter (POST /v1beta/models/{model}:generateContent and :streamGenerateContent). Use for Gemini SDKs or the Gemini CLI pointed at FastRouter.
---

# FastRouter: Gemini

Base URL `${FASTROUTER_URL}`, key in `x-goog-api-key`, `?key=` or `Authorization: Bearer`.

## Request

```bash
curl "$FASTROUTER_URL/v1beta/models/ds/deepseek-chat:generateContent" \
  -H "x-goog-api-key: $FASTROUTER_KEY" -H "Content-Type: application/json" \
  -d '{"contents": [{"role": "user", "parts": [{"text": "hello"}]}]}'
```

- The model goes in the path: `/v1beta/models/<prefix/model or combo>:generateContent`.
- `:streamGenerateContent?alt=sse` streams SSE chunks.
- `GET /v1beta/models` lists models in Gemini's format.
- Non-Gemini upstreams are translated (function calls and inline images included).

## Gemini CLI

```bash
export GOOGLE_GEMINI_BASE_URL="http://localhost:20128"
export GEMINI_API_KEY="$FASTROUTER_KEY"
gemini -m "gemini/gemini-4-argon"   # any FastRouter model or combo
```
