# Anonymous XMPP and 9p on ch.at

Design for the follow-up to PR #60. These are native, public, anonymous services,
not account-based bots, localhost-only demos, or protocols hidden behind a
mandatory encryption bridge. Implementation/tests precede any claim they are live.

## User-facing entry points

| Protocol | Main address | Login | Encryption |
| --- | --- | --- | --- |
| XMPP client-to-server | `ch.at:5222`, chat with `chat@ch.at` | SASL ANONYMOUS; no account/password | Plain TCP works; STARTTLS is optional |
| 9p2000 | `tcp!ch.at!564` | Anonymous attach; no account/password | Plain TCP works |

Use the usual ports so native clients do not need a custom transport or new
client-side bridge. No separate XMPP server/account, DNS delegation or certificate
is required for basic operation. Optional encrypted access must not make the plain
native paths unusable. Users should use the existing HTTPS/SSH service for secrets.

Add `XMPP_PORT` and `NINEP_PORT` beside the existing port constants in `chat.go`;
zero disables a service. Start them the same way as SSH/DNS/Gopher. Optional XMPP
STARTTLS can use the existing certificate if available, but absence of that
certificate does not disable anonymous XMPP. Do not require PLAIN/password login.

A code change can bind these ports, but cannot make a blocked firewall port
reachable. Before advertising production availability, check 5222/564 externally.
Any required firewall/network change needs explicit approval; do not silently
open ports or claim that a successful local test proves public reachability.
The follow-up remains a PR, not an automatic merge/deployment.

## XMPP service boundary

Use standard XML streams, SASL ANONYMOUS and resource binding. Generate a random
session JID under `ch.at`; client-supplied names/from fields must not become trusted
identities. Accept messages only for the local chatbot `chat@ch.at`, and return
responses on the same session. No federation, registration, arbitrary routing,
MUC, public user directory, file upload or offline queues: this is an anonymous
XMPP interface to the chatbot, not an open relay or a new messaging product.

Support normal control traffic: presence, resource binding, ping, service
discovery and clear IQ errors. Do not answer error stanzas or empty/non-chat
messages, avoiding reply loops. One prompt produces one answer; don't silently
accumulate expensive conversation history. An XMPP client supporting anonymous
login should be able to connect to the ordinary port without an API key, JID
registration, browser or custom Go client.

## 9p filesystem

Export a private virtual filesystem per connection with `ctl` and `README`.
There is no mapping to the server's host filesystem. A user can mount with a
normal 9p2000 client, write a newline-terminated prompt to `ctl`, then read the
answer from `ctl`. Support fragmented writes, normal byte offsets, directory
listing, short reads/EOF, multiple fids, stat, clunk and flush.

A completed write accepts a prompt; it must not generate twice on duplicate or
fragmented writes. Reads wait for that answer. Clunking the writer after `echo`
must not discard the response before a reader can retrieve it. Closing the whole
connection cancels pending work. A new prompt may replace the previous answer
only once that request completes. Separate connections cannot see each other's
prompts/results. Reject auth/create/remove/wstat/unknown paths rather than exposing
real filesystem operations or inventing a login system.

## Style and limits: reuse existing patterns

- Keep source configuration and small protocol handlers like the other services.
  Use established protocol codecs/negotiation rather than inventing a new wire format.
- Reuse `rateLimitAllow` with the real peer address; do not trust forwarded headers,
  claimed XMPP JIDs or 9p usernames as a way around IP limits.
- Use the existing SSH pattern of at most 100 active connections and one active
  prompt per session. Avoid a new account/quota/configuration subsystem.
- Reuse the 8192-byte input bound and a 60-second model deadline, consistent with
  existing transport sizes/timeouts. Bound retained answers at 64 KiB.
- Add only necessary protocol bounds: frame/stanza size, 64 fids per 9p connection,
  bounded pending reads, and handshake/read/write deadlines. Reject malformed
  input before invoking the model. Test disconnect/flush cleanup and isolation.
- Do not log prompt/answer bodies, anonymous trace data or upstream secrets.
  Return generic protocol errors rather than raw provider errors. No chat database.

## Function calls / OpenCode in the same follow-up

The structured API must preserve tool definitions/JSON schemas, choice mode,
assistant tool-call IDs, incremental argument deltas, tool-result messages,
usage and real upstream finish reasons. Tools execute in the user's client,
never on ch.at's server. One API request makes at most one upstream request.

The private deployed text adapter cannot invent structured tool calls. Add a
real configured structured upstream path, keep existing text transports/builds
working, and document its operator configuration instead of pretending a text
fallback implements agent support. Do not change deployed credentials during
this PR. Real OpenCode interoperability must be tested with an isolated CLI
agent and local deterministic upstream: tool call -> read a disposable fixture ->
submit matching tool result -> final answer. SDK text completion alone is not this test.

## Acceptance gates

1. Plain TCP XMPP: a real client negotiates ANONYMOUS/binding, sends a chat stanza,
   receives its answer, and disconnects. Optional TLS must not be required. Test
   spoofed senders, foreign targets, ping/discovery, malformed XML and limits.
2. Plain TCP 9p: a real 9p client attaches without auth and performs the normal
   write/read workflow. Test fids, fragmentation, offsets, flush, malformed frames,
   concurrent connections, rate limits and cancellation during model work.
3. Race tests/vet and bounded-work checks, with local fake models rather than paid
   inference. Preserve existing HTTP/SSH/DNS/board behavior.
4. Actual OpenCode tool/result/final-answer cycle and structured API stream tests.
5. README/homepage instructions show native ports and anonymous usage. Public
   reachability remains a separate deployment check, not an inferred success.
6. Before a later authorized merge, check/back up the RAM-only agent feed. A public
   export is not full-state persistence and cannot recover an earlier lost feed.
