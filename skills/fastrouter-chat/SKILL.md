---
name: fastrouter-chat
description: Chat and code generation through FastRouter's OpenAI-compatible POST /v1/chat/completions, with streaming, tools and images. Use for any OpenAI SDK or client pointed at FastRouter.
---

# FastRouter: Chat Completions

Base URL `${FASTROUTER_URL}/v1`, key `Authorization: Bearer ${FASTROUTER_KEY}` (see the `fastrouter` skill for setup).

## Request

```bash
curl $FASTROUTER_URL/v1/chat/completions \
  -H "Authorization: Bearer $FASTROUTER_KEY" -H "Content-Type: application/json" \
  -d '{
    "model": "ds/deepseek-chat",
    "messages": [{"role": "user", "content": "Write a haiku about routers"}],
    "stream": false
  }'
```

- `model`: `prefix/model` or a combo name from `GET /v1/models`.
- `stream: true` returns Server-Sent Events (`data: {...}` chunks, then `data: [DONE]`).
- `tools`, `tool_choice`, `response_format`, `temperature`, `max_tokens` and image parts (`{"type":"image_url"}`) pass through. If the chosen model cannot see images and a vision fallback is configured, FastRouter routes image requests there.
- Reasoning: `reasoning_effort` is passed on; per-provider thinking defaults are set in Dashboard → Settings.

## OpenAI SDK

```python
from openai import OpenAI
client = OpenAI(base_url="http://localhost:20128/v1", api_key="sk-...")
r = client.chat.completions.create(model="ds/deepseek-chat", messages=[{"role": "user", "content": "hi"}])
print(r.choices[0].message.content)
```

```js
import OpenAI from "openai";
const client = new OpenAI({ baseURL: "http://localhost:20128/v1", apiKey: process.env.FASTROUTER_KEY });
const r = await client.chat.completions.create({ model: "my-combo-team", messages: [{ role: "user", content: "hi" }] });
```

## Notes

- Upstreams that only speak Anthropic, Responses or Gemini are translated automatically.
- On an error or rate limit before the first byte, FastRouter retries the next account, then the next model of a combo.
