---
name: fastrouter
description: Entry point for FastRouter, a local AI gateway (a faster Go version of 9Router) that speaks the OpenAI, Anthropic, OpenAI Responses and Gemini APIs on one port. Use when the user mentions FastRouter, FASTROUTER_URL, or wants to call many LLM providers through one local endpoint. Covers setup, auth, model discovery, prefixes and combos, and links to the per-API skills.
---

# FastRouter

One local endpoint in front of many LLM accounts (API keys, Codex / GLM sign-ins, custom OpenAI- or Anthropic-compatible endpoints). It picks an account, falls back to the next one on errors or rate limits, and translates between API formats.

## Setup

```bash
export FASTROUTER_URL="http://localhost:20128"   # the dashboard shows the real address
export FASTROUTER_KEY="sk-..."                   # Dashboard → API Keys; omit if no keys are set
```

- Send the key as `Authorization: Bearer $FASTROUTER_KEY` (OpenAI style), `x-api-key` (Anthropic style) or `x-goog-api-key` / `?key=` (Gemini style). All work on every route.
- With no API keys configured, FastRouter only accepts requests from the same machine (localhost).
- Check it is up: `curl $FASTROUTER_URL/health` → `{"status":"ok","usableAccounts":N,...}`

## Discover models

```bash
curl -H "Authorization: Bearer $FASTROUTER_KEY" $FASTROUTER_URL/v1/models
```

Use `data[].id` as the `model` field. Ids look like `prefix/model`:

- The **prefix** picks the provider: `ds/deepseek-chat` (DeepSeek), `cx/gpt-5.5` (OpenAI Codex sign-in), `glm/glm-5`, `openrouter/...`, or a custom endpoint's own prefix.
- A **combo** is a name with no prefix (for example `my-combo-team`). FastRouter tries its models with the combo's strategy: fallback (in order), round-robin, smart (a router model picks) or fusion (several models answer, a judge merges).
- A model that is not listed still works if the provider serves it: `prefix/any-model-id`.

## Which API to use

Pick whatever format your client already speaks; FastRouter translates when the upstream differs.

| API | Endpoint | Skill |
|---|---|---|
| OpenAI Chat Completions | `POST /v1/chat/completions` | https://raw.githubusercontent.com/nesaon03/fastrouter/refs/heads/main/skills/fastrouter-chat/SKILL.md |
| Anthropic Messages | `POST /v1/messages` | https://raw.githubusercontent.com/nesaon03/fastrouter/refs/heads/main/skills/fastrouter-messages/SKILL.md |
| OpenAI Responses | `POST /v1/responses` | https://raw.githubusercontent.com/nesaon03/fastrouter/refs/heads/main/skills/fastrouter-responses/SKILL.md |
| Gemini generateContent | `POST /v1beta/models/{model}:generateContent` | https://raw.githubusercontent.com/nesaon03/fastrouter/refs/heads/main/skills/fastrouter-gemini/SKILL.md |
| Models, prefixes and combos | `GET /v1/models` | https://raw.githubusercontent.com/nesaon03/fastrouter/refs/heads/main/skills/fastrouter-models/SKILL.md |

If FastRouter runs on this machine, the same files are served at `$FASTROUTER_URL/skills/<name>/SKILL.md`.

## Useful headers

- Response `X-FastRouter-Model` and `X-FastRouter-Account`: which model and account answered.
- Request `X-FastRouter-Token-Saver: off`: skip the token savers (prompt compression) for one request.

## Errors

- 401 `authentication_error` → missing or wrong key (Dashboard → API Keys).
- 403 `permission_error` → this key may not use that model, or the request came from another machine with no keys set.
- 404 `model_not_found` → unknown prefix or combo; list `/v1/models`.
- 429 `rate_limit_error` → this key's own rate limit; wait and retry.
- 502 `api_error` → every account for that model failed; the message names the last upstream error.
