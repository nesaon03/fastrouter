---
name: fastrouter-models
description: How FastRouter names models: provider prefixes, custom endpoints, combos and their strategies, and how to list what is available (GET /v1/models). Use when choosing a model id for FastRouter or debugging model_not_found.
---

# FastRouter: Models, prefixes and combos

## List

```bash
curl -H "Authorization: Bearer $FASTROUTER_KEY" $FASTROUTER_URL/v1/models
# {"object":"list","data":[{"id":"ds/deepseek-chat","object":"model","owned_by":"fastrouter"}, ...]}
```

Only providers with at least one working account are listed.

## Model ids

`<prefix>/<model>`. The prefix is the provider's alias or id, shown on each provider card in the dashboard:

| Prefix | Provider |
|---|---|
| `ds` / `deepseek` | DeepSeek |
| `cx` / `codex` | OpenAI Codex (sign-in) |
| `glm` | Z.ai GLM Coding |
| `openrouter` | OpenRouter |
| `ocg` / `opencode-go` | OpenCode Go |
| `gemini`, `openai`, `anthropic`, `xai`, `nvidia`, `groq`, `mistral`, ... | the provider's id |
| your own (e.g. `neon`) | a custom OpenAI- or Anthropic-compatible endpoint |

Models a provider serves but FastRouter does not list still work: `prefix/exact-upstream-id`.

## Combos

A combo is a named list of models with no prefix, made in Dashboard → Combos:

- **fallback**: try models in order until one answers.
- **round-robin**: spread requests across the models.
- **smart**: a small router model picks the best model for each request.
- **fusion**: several models answer and a judge model merges the best answer.

Use the combo name as `model`, e.g. `"model": "my-combo-team"`.

## When a model fails

FastRouter retries the next account of the same provider, then the next model of the combo. Accounts that keep failing cool down for a while (see Dashboard → Quota Tracker).
