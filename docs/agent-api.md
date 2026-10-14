# Structured tool calls and OpenCode agents

The OpenAI-compatible `/v1/chat/completions` route supports structured function
calls when an operator configures a compatible upstream. Tools run in the user's
client, **never on the ch.at server**. The server does not execute tool names,
shell commands, filesystem operations, or URLs found in model output.

## Operator setup: reuse the existing provider

The public `llm.go.example` now registers the same URL/key/model for both its
legacy text adapter and the structured API. A clean self-hosted checkout that
copies/configures this example needs no second provider or credentials.

An existing private `llm.go` remains build-compatible. To enable its structured
API, either add this initialization hook using that adapter's existing settings:

```go
func init() {
    configureStructuredChat(apiURL, apiKey, modelName)
}
```

Or configure the process with:

- `CHAT_API_URL`: the full existing OpenAI-compatible **chat completions** URL,
  such as `https://api.openai.com/v1/chat/completions` or an existing local endpoint.
- `CHAT_API_KEY`: that endpoint's existing key; empty is allowed for an anonymous
  local upstream. This value is never taken from a public client's Authorization header.
- `CHAT_MODEL`: the operator-selected model with actual tool-call support.

An explicit `CHAT_API_URL` takes precedence over the initialization hook and
requires its own `CHAT_MODEL`. The public request's `model` string is a client
identifier, not permission to select a more expensive upstream model. This does
not add model selection or provision a new paid service.

Without either configuration path, the legacy text API still works, but tool
requests receive a clear configuration error. **Merging code does not configure
the deployed private adapter or its secrets.** Production activation must include
that operator step; do not advertise full agent support until it is verified.

## What crosses the API

The structured route preserves:

- `tools` with JSON schemas, `tool_choice` (including named/required selection),
  and `parallel_tool_calls`;
- assistant `tool_calls`, their IDs, function names and JSON argument strings;
- subsequent `role: "tool"` messages with the matching `tool_call_id`;
- streaming tool-call indexes and incremental argument fragments;
- upstream IDs, model metadata, usage, and real `finish_reason` values such as
  `tool_calls`, `stop`, and `length`.

A client sends a normal messages/tools request, executes only the operations the
user permits, then sends the complete conversation including the assistant's
original tool calls and the corresponding tool results. That next API request
can produce the final answer or another tool call. No tool execution loop or
conversation database runs on the server.

Example first request (the client implements `get_time` locally):

```json
{
  "model": "default",
  "messages": [{"role": "user", "content": "What time is it on my computer?"}],
  "tools": [{
    "type": "function",
    "function": {
      "name": "get_time",
      "description": "Get the user's local clock time",
      "parameters": {"type": "object", "properties": {}, "additionalProperties": false}
    }
  }],
  "tool_choice": "auto",
  "stream": true
}
```

The actual tool-call ID returned by the model must be used in the tool-result
message; never invent a different ID or drop the assistant tool-call message.
If `finish_reason` is `length`, treat the answer/arguments as potentially
incomplete rather than executing a truncated JSON argument string.

## OpenCode configuration

Use the custom provider as the selected model—not an unrelated free provider.
For example, in an OpenCode project configuration:

```json
{
  "$schema": "https://opencode.ai/config.json",
  "model": "chat/default",
  "small_model": "chat/default",
  "enabled_providers": ["chat"],
  "provider": {
    "chat": {
      "npm": "@ai-sdk/openai-compatible",
      "name": "ch.at",
      "options": {
        "baseURL": "https://ch.at/v1",
        "apiKey": "anonymous"
      },
      "models": {
        "default": {
          "name": "ch.at configured backend",
          "tool_call": true,
          "limit": {"context": 32768, "output": 500}
        }
      }
    }
  },
  "permission": {"*": "ask", "read": "allow"}
}
```

`anonymous` is not a secret or account: the public ch.at endpoint requires no
client API key. Set context capability to match the operator's actual model.
This example assumes the structured backend has been activated on that server.
Keep OpenCode's permission prompts enabled for writes/commands; anonymous access
to the model is not authorization to run arbitrary tools on your computer.

```sh
opencode run --model chat/default 'Read README.md and summarize it. Do not modify files.'
```

## Limits and cost behavior

The structured route reuses the existing peer-IP rate limiter, limits request
bodies to 1 MiB and responses to 8 MiB, and imposes a 60-second upstream deadline.
Only `n=1` is accepted. Output-token requests are capped at 500 (the existing
example adapter's budget), and omitted limits get that same cap. Adjust the
source constant deliberately if an operator later chooses a different budget.
Large generated patches may hit this limit; it is not unlimited coding inference.

Each API call makes **one** upstream request, with no fallback or hidden retry.
An agent task may naturally require several API calls to return tool results and
continue, so it can consume more tokens than a single text question. These limits
do not make agent workloads free; they avoid introducing unbounded server loops
or silent selection of a pricier model.

Provider errors, malformed/truncated streams and redirects are not treated as
successful completions. Error bodies/URLs/credentials are not relayed. Disconnect
cancels upstream work. Client Authorization values are not forwarded upstream.
The default HTTP client verifies HTTPS certificates; local HTTP upstreams are
also supported if explicitly configured by the operator.

## Verified acceptance test

`TestOpenCodeAgentToolRoundTrip` uses the real OpenCode CLI, an isolated home/config
and a disposable local fixture. Its deterministic local upstream requests the
client's `read` tool with arguments split across stream chunks, observes the
matching tool result containing the fixture's marker, and then sends a final
answer. File writes, shell tools and external-directory access are denied in the
test. No paid model calls or user credentials are used.

Verified with **OpenCode 1.19.0**. To repeat with a locally installed CLI:

```sh
CHAT_OPENCODE_BIN="$(command -v opencode)" \
    go test -race -tags boardtest -run TestOpenCodeAgentToolRoundTrip -v
```

The regular Go suite also covers schemas/results, incremental argument deltas,
finish reasons/usage, limits, credential separation, cancellation and redirects.
This verifies the actual agent tool/result loop, not merely text-stream parsing;
live provider behavior and production configuration still need deployment checks.
