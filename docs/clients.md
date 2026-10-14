# Small client recipes

These use the existing HTTP/DNS service: no new ports, provider, paid model,
account, or server-side conversation store. Features described here require a
server running this revision; opening a PR does not deploy them.

## Answer-only terminal output (#24, #37)

`raw=1` returns only the streamed answer as UTF-8 plain text, without the `Q:` /
`A:` labels, question echo, HTML or an extra trailing newline. It overrides
browser detection and Accept negotiation. The default output remains compatible.
Use proper URL encoding, not string concatenation of an unescaped prompt:

```sh
curl --fail --silent --show-error --no-buffer --get \
  --data-urlencode 'q=What does a DNS resolver do? Be brief.' \
  --data-urlencode 'raw=1' https://ch.at/
```

### Bash / Zsh

Add this to your shell profile (requires only `curl`):

```bash
chat() {
    if [ "$#" -eq 0 ] || [ -z "$*" ]; then
        printf 'Usage: chat "your question"\n' >&2
        return 2
    fi
    curl --fail --silent --show-error --no-buffer --get \
        --data-urlencode "q=$*" --data-urlencode 'raw=1' https://ch.at/
}

chat 'What does "a & b" mean? Keep it short.'
```

Quote prompts containing shell metacharacters. Output is the answer only;
HTTP/network errors produce a nonzero exit status. If you want a shell newline,
append `; printf '\n'` to the invocation. Nothing is logged automatically.

### PowerShell

Add this function to your PowerShell profile:

```powershell
function chat {
    param(
        [Parameter(Mandatory = $true)]
        [ValidateNotNullOrEmpty()]
        [string]$Message
    )
    $query = [uri]::EscapeDataString($Message)
    Invoke-RestMethod -Uri "https://ch.at/?raw=1&q=$query" -Method Get -ErrorAction Stop
}

chat 'What does "a & b" mean? Keep it short.'
```

PowerShell displays the completed response; the Bash/curl recipe streams it.

## System prompts, context and conversation history (#9)

Send the complete desired history in `messages`. The server does not remember
an earlier HTTP request for you. Roles and content are forwarded to the configured
adapter; keep context concise to avoid spending tokens unnecessarily.

```sh
curl --fail --silent --show-error https://ch.at/v1/chat/completions \
  -H 'Content-Type: application/json' \
  --data '{
    "model": "default",
    "messages": [
      {"role": "system", "content": "You are a concise assistant."},
      {"role": "user", "content": "My project uses Go."},
      {"role": "assistant", "content": "Understood."},
      {"role": "user", "content": "Suggest a standard-library HTTP testing tool."}
    ]
  }'
```

The `model` string is a client-facing identifier, not paid model selection: this
server uses its single configured backend. These changes do not enable any new
models. A self-hosted instance can use an **already running** llamafile or another
OpenAI-compatible local backend by configuring `llm.go`'s `apiURL`, `apiKey` and
`modelName`. See `llm.go.example`; native Anthropic and native Ollama API formats
are not interchangeable with OpenAI's `/v1/chat/completions` protocol.

## Text streaming clients / OpenCode (#49)

For `/v1/chat/completions`, set `stream: true`. A configured structured backend
preserves tool calls, argument deltas, tool-result messages, usage and the real
upstream finish reason (`tool_calls`, `stop`, `length`, etc.), followed by
`data: [DONE]`. See the [agent API guide](agent-api.md) for operator activation,
OpenCode configuration and the verified real CLI tool/result round trip.

The issue's configuration selected `opencode/glm-4.7-free`, not its custom `chat`
provider. Select `chat/default` and configure `@ai-sdk/openai-compatible` with
`https://ch.at/v1`; enable tool-call capability for that client model. The guide
includes a complete configuration and client-side permission recommendations.

The old text-only adapter remains compatible when no structured backend is
configured. It emits an initial assistant role, text deltas, terminal
`finish_reason: "stop"` and `[DONE]`, but cannot expose upstream usage or distinguish
its true stop/length reason. Tools in this fallback receive a clear configuration
error rather than being silently ignored. Do not claim agent support is live merely
because code was merged: the deployed provider must be configured and support tools.

The root endpoint also supports `Accept: text/event-stream`, using plain-text
SSE data rather than OpenAI JSON. It invokes the model only once; multiline
chunks are framed safely. Use `/v1/chat/completions` for OpenAI-compatible clients.

## Edit a web conversation (#8)

Each question has **Edit this question → Regenerate from here**. Edit the text
and submit: only the earlier history and your edited question are sent to the
model. That answer and all later turns are discarded in the new conversation.
This makes one model call, just like sending a new question; editing the page
alone makes no call. There is no automatic background regeneration.

The footer also exposes an advanced **Edit conversation history** transcript.
Keep its `Q:` / `A:` line markers intact. **Send** continues from that transcript;
**Regenerate last answer** replaces its final answer. These are simple textual
markers, not a structured conversation editor: literal marker lines inside a
message can be ambiguous. History remains client-side, bounded at 64KB on input,
and is not saved across closing the page. A single shared form avoids embedding
a growing history copy for every turn. Untrusted history/model HTML is escaped,
except for a small allowlist of attribute-free formatting tags.

## DNS prompts longer than one label (#33)

The limit is **63 bytes per label**, not 63 characters per question. Split words
across labels beneath `ask.ch.at`; both dots and hyphens become spaces:

```sh
dig explain.the.difference.between.a.recursive.dns.resolver.and.an.authoritative.dns.server.ask.ch.at TXT
```

This question exceeds 63 bytes, but each label fits. The entire wire-format name
is still limited to 255 bytes (normally at most 253 textual characters without
the trailing dot), **including** `.ask.ch.at`. There is no unlimited-length DNS
prompt or paging feature. For longer requests, use HTTPS or SSH.

## Unicode and exact-case DNS prompts (#29)

Use the explicit `_b32` namespace with **unpadded base32 of UTF-8 bytes**. Split
the encoded payload into labels of at most 63 characters:

```sh
name=$(python3 - <<'PY'
import base64
text = 'Translate café to 日本語'
payload = base64.b32encode(text.encode('utf-8')).decode('ascii').rstrip('=').lower()
name = '_b32.' + '.'.join(payload[i:i+63] for i in range(0, len(payload), 63)) + '.ask.ch.at'
if len(name) > 253:
    raise SystemExit('Prompt is too long for DNS; use HTTPS or SSH')
print(name)
PY
)
dig "$name" TXT
```

Base32 tolerates resolver case randomization while preserving the **decoded**
text's original case, punctuation and Unicode. Ordinary prompts are not guessed
to be encoded. Invalid encoding, noncanonical padding bits and invalid UTF-8 are
rejected without a model call. DNS is not an encrypted or private channel; this
is encoding, not encryption. Encoding expands the request, so Unicode prompts
may hit DNS's total-name limit sooner. Resolver timeouts remain possible, as with
ordinary `ask.ch.at` queries; these changes do not claim to fix that reliability
issue. TXT response bytes are UTF-8, though tools such as `dig` may display some
bytes with escapes; use a DNS client/library to join and decode the TXT strings.
