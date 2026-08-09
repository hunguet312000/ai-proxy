# Cursor: how the integration works, and how to repair it

LiteRouter talks to Cursor over the private protocol its IDE uses. There is no public
API, no published schema, and no versioning promise. Every field number in
`internal/pool/oauth/cursor_agent.go` was read out of the IDE's own JavaScript bundle,
and an IDE update can move any of them.

This document exists so that repairing the integration is a procedure rather than a
rediscovery. It records what is true, how each fact was obtained, and — just as
usefully — which plausible explanations have already been measured and ruled out.

---

## 1. The shape of it

| | |
|---|---|
| Endpoint | `POST https://agent.api5.cursor.sh/agent.v1.AgentService/Run` |
| Protocol | Connect, `content-type: application/connect+proto`, HTTP/2 |
| Framing | `[flags:1][len:4 big-endian][payload]`; flag `0x02` = end-of-stream, payload is JSON |
| Auth | A session copied out of the IDE — no OAuth flow, no refresh token |
| Code | `cursor_agent.go` (protocol), `cursor_cache.go` (conversation state), `cursor.go` (headers/session), `cursor_detect.go` (import) |

**The request body is a pipe, not a buffer.** The service reads its own conversation
state back mid-turn (§4), and a half-closed request stream cannot answer. Sending the
first frame and closing works only for a conversation with no prior state.

**`aiserver.v1.ChatService` on `api2.cursor.sh` is dead** for inference. That is the
endpoint 9router uses and the one this integration originally targeted. See §7.

---

## 2. First response when something breaks

Run the live probe before reading any code. It uses the production encoder and decoder,
so whatever it shows is what the gateway would do.

```bash
LITEROUTER_CURSOR_LIVE=1 \
LITEROUTER_CURSOR_PROBE_AGENT_MODELS=composer-2.5-fast \
go test ./internal/pool/oauth/ -run 'TestCursorLiveProbe/agent' -v -count=1 -timeout 180s
```

A healthy run prints OpenAI-shaped chunks ending in `finish_reason` and `[DONE]`.

### Symptom → cause

| What you see | What it means | Where to look |
|---|---|---|
| `parse binary: illegal tag: field no N wire type 7` | A field number or wire type is wrong. The service is reading your bytes as a different field. | §3, then re-dump the message that owns field N |
| Hangs, no output | The turn produced a tool call and no terminator arrived, or the request stream closed too early | §5 |
| `Update Required: Your version of Cursor is no longer supported` | Almost certainly **not** the version. See §7. | §7 |
| `Named models unavailable: Free plans can only use Auto` | Plan restriction, honest message. Only `composer-*` and `default` on free. | §6 |
| Answers as if the conversation never happened | Conversation state replay is broken — the state was accepted but not reconstructed | §4 |
| Tool call arrives with empty name or `{}` arguments | `McpArgs` layout moved | §3 |
| `HTTP 415` | A unary call was sent with the streaming envelope, or vice versa | §6 |

---

## 3. Recovering the schema after an IDE update

The IDE ships generated protobuf-es descriptors. `tools/cursor-schema.py` reads them
back:

```bash
python3 tools/cursor-schema.py agent.v1.AgentRunRequest
python3 tools/cursor-schema.py agent.v1.ConversationStateStructure agent.v1.McpArgs
```

Bundle path defaults to `/Applications/Cursor.app/.../workbench.desktop.main.js`;
override with `LITEROUTER_CURSOR_BUNDLE`.

### Field numbers this integration depends on

Check these first. Each is a constant in `cursor_agent.go` or `cursor_cache.go`.

**`agent.v1.AgentClientMessage`** — what we send
| # | field | used for |
|---|---|---|
| 1 | `run_request` | the opening frame |
| 3 | `kv_client_message` | blob replies (§4) |

**`agent.v1.AgentRunRequest`**
| # | field | note |
|---|---|---|
| 1 | `conversation_state` | empty on a fresh conversation, replayed state otherwise |
| 2 | `action` | `ConversationAction{1: user_message_action}` |
| 3 | `model_details` | `{1: model_id, 3: display_model_id, 4: display_name}` |
| 5 | `conversation_id` | |
| 9 | `requested_model` | `{1: model_id}` — must agree with `model_details` |

**`agent.v1.McpToolDefinition`** — declaring the caller's tools
| # | field | note |
|---|---|---|
| 1 | `name` | |
| 2 | `description` | |
| 3 | `input_schema` | **a Struct — do not put JSON here.** Raw JSON produces `illegal tag` |
| 4 | `provider_identifier` | `"literouter"` |
| 5 | `tool_name` | |
| 6 | `input_schema_json` | the JSON schema goes here |

**`agent.v1.McpArgs`** — decoding a tool call
| # | field | note |
|---|---|---|
| 1 | `name` | |
| 2 | `args` | **`map<string, google.protobuf.Value>`.** Each entry is its **own occurrence** of field 2. Decoding only the first drops every argument but one |
| 3 | `tool_call_id` | |
| 5 | `tool_name` | preferred over `name` when set |

**`agent.v1.AgentServerMessage`** — what we read
| # | field | used for |
|---|---|---|
| 1 | `interaction_update` | model output |
| 2 | `exec_server_message` | treated as idle (§5) |
| 4 | `kv_server_message` | blob traffic (§4) |

**`agent.v1.InteractionUpdate`**
| # | field | note |
|---|---|---|
| 1 | `text_delta` | `{1: text}` |
| 2 | `tool_call_started` | `{1: call_id, 2: ToolCall}`; only `ToolCall.15 mcp_tool_call` is handled |
| 4 | `thinking_delta` | |
| 8 | `token_delta` | `{1: tokens}` — an **increment**, not a total. Verified: 31 tokens summed for an 80-character completion |
| 13 | `heartbeat` | idle |
| 14 | `turn_ended` | the terminator |

**`agent.v1.ConversationStateStructure`** — the cache (§4)
| # | field | note |
|---|---|---|
| 1 | `root_prompt_messages_json` | repeated **blob ids**, 32 bytes each |
| 8 | `turns` | repeated **blob ids** of `ConversationTurnStructure` |
| 22 | `agent_type` | `"ide"` — used to recognise the state blob among the others |

---

## 4. Conversation state — where the token saving comes from

The service keeps no conversation state of its own. It streams state to the client as
blobs and reads them back:

```
server → kv_server_message.set_blob_args {blob_id, blob_data}   we store
server → kv_server_message.get_blob_args {blob_id}              we answer
```

On the next turn LiteRouter replays the state and sends only the new messages, so the
upstream payload is O(new turn) rather than O(history). Measured over four turns while
history grew from 4.3 KB to 18.5 KB: **5.2 KB with the cache on, 18.7 KB off.**

### The part that is easy to get wrong

Replaying `conversation_state` with only `turns` appended — the obvious reading of the
schema — makes the model answer as if the conversation never happened, **with no error
anywhere**. The service accepts the state, fetches the turn blob, and ignores it.

The correct shape came from the IDE's own stored state, not from the schema:

```bash
sqlite3 "$HOME/Library/Application Support/Cursor/User/globalStorage/state.vscdb" \
  "SELECT value FROM cursorDiskKV WHERE key LIKE 'composerData:%' LIMIT 5"
# conversationState is base64 protobuf; decode and compare against what commit() builds
```

A real post-turn state carries the turn's messages appended to
`root_prompt_messages_json` **in addition to** the turn id in `turns`. Blob write order
is the root order. `commit()` in `cursor_cache.go` reproduces this.

**Always copy the database before reading it.** Opening it in place — even `mode=ro` —
can checkpoint the WAL and rewrite a 350 MB file belonging to a running application.
`readCursorState` copies the db plus `-wal` and `-shm` to a temp dir first, and
`TestReadCursorStateLeavesTheOriginalUntouched` holds that line.

### Safety rules the cache must keep

- Continue only when the stored fingerprints are a **strict prefix** of the request's.
  Compaction, an edit or a branch drops the entry and rebuilds from scratch.
- Strip leading **assistant** turns from the delta (the service recorded its own reply);
  never strip **tool** results (those come from the client).
- Bounded by bytes, not entry count: 8 MB per conversation, 128 MB total. One folded
  transcript can be hundreds of kilobytes.
- Off with `LITEROUTER_CURSOR_CACHE=0`.

---

## 5. Terminators — why a turn can hang forever

`turn_ended` (update 14) ends a normal turn. **After a tool call it never arrives**: the
service waits for a tool result on the same stream, which a stateless proxy will not
send. The only signals that follow are `exec_server_message` and heartbeats.

So: an idle frame **after** at least one tool call ends the turn; an idle frame **before**
any content is just a keep-alive and must not. Both directions are covered by
`TestAgentStreamEndsTheTurnWhenAToolCallLeavesItIdle` and
`TestAgentStreamKeepsRunningThroughIdleFramesBeforeAnyToolCall`.

Reading to EOF hangs indefinitely — the service keeps heartbeating after the turn.

---

## 6. Asking the service what it will accept

Both are unary calls: bare protobuf body, `content-type: application/proto`, **not** a
Connect envelope (an envelope gets HTTP 415).

```bash
# what this account may actually call
LITEROUTER_CURSOR_LIVE=1 \
LITEROUTER_CURSOR_PROBE_BASE_URL=https://agent.api5.cursor.sh \
LITEROUTER_CURSOR_PROBE_UNARY=/agent.v1.AgentService/GetUsableModels \
go test ./internal/pool/oauth/ -run 'TestCursorLiveProbe/unary' -v -count=1

# the full catalogue, and one model's entry field by field
LITEROUTER_CURSOR_LIVE=1 \
LITEROUTER_CURSOR_PROBE_MODEL_DETAIL=composer-2.5-fast \
go test ./internal/pool/oauth/ -run 'TestCursorLiveProbe/unary' -v -count=1
```

`GetUsableModels` lists what the **service** offers, not what the **plan** allows. On a
free plan only `composer-2.5`, `composer-2.5-fast` and `default` return completions;
everything else answers `Named models unavailable: Free plans can only use Auto`.

---

## 7. Dead ends — measured, not assumed

Do not re-walk these.

**`api2.cursor.sh` / `aiserver.v1.ChatService` is closed to inference.** Every request
returns `resource_exhausted` with the text *"Your version of Cursor is no longer
supported"*. Decoding the protobuf in `details[].value` shows the real code is enum
**28** `ERROR_GPT_4_VISION_PREVIEW_RATE_LIMIT` (`actionRequired: payment`) or enum **51**
`ERROR_RATE_LIMITED_CHANGEABLE` (`upgrade`). `ERROR_OUTDATED_CLIENT` is enum **30** and
is never sent. The message is boilerplate.

**The client version is not the problem.** The same session replayed while claiming
`3.12.17`, `3.15.6` and `9.9.9` returns a byte-identical refusal:

```bash
LITEROUTER_CURSOR_LIVE=1 LITEROUTER_CURSOR_PROBE_VERSION=9.9.9 \
LITEROUTER_CURSOR_PROBE_COMMIT=<any> ... # see the probe's env knobs
```

**Also ruled out on api2:** `machineId` form (`serviceMachineId`, `telemetry.machineId`,
and the macOS `machineId/macMachineId` pair), the `x-cursor-client-commit` header (the
IDE only sends it for Anysphere staff), an Electron user-agent, `x-ghost-mode:
implicit-false`, `x-cursor-client-os-version`, `x-cursor-client-layout`, and hosts
`api3`/`api4` (both 404).

**`x-inference-authentication-jwt`** is set only from an internal `INFERENCE_PROXY_JWT`
environment variable. It is not required.

**A sticky bidi stream does not give multi-turn.** Sending a second
`conversation_action` on an open `Run` stream after `turn_ended` produces nothing; the
service closes the run.

---

## 8. Client identity

`x-cursor-checksum` = six bytes of `unixMilli/1e6` through a rolling XOR (seed 165),
encoded with Cursor's own base64 alphabet (`-_` at 62/63, no padding), with the machine
id appended. It changes every ~17 minutes, so it identifies the client rather than
acting as a nonce. `x-client-key` = sha256(token). `x-session-id` = UUIDv5(token, DNS).

The build is read from the installed IDE's `product.json` and stored **on the account**,
so token and client identity always come from the same install. `/Applications` is
mounted read-only at `/host-apps` by `docker-compose.yml` for this.

On macOS the IDE appends `/macMachineId` to the checksum. LiteRouter does not, and it
made no difference on api2 — untested on api5.

---

## 9. Usage accounting

Cursor reports **completion tokens only**, and no cache figures at all. LiteRouter
supplies its own estimate of the prompt it actually sent — the delta when a conversation
is replayed — tagged `prompt_tokens_estimated`, which keeps the dashboard's `~` marker
honest. "Estimated" means *the upstream did not report it*, not *the value was zero*.

Effort is **not** sent on this path: the agent request carries no such field. The Models
tab hides the reasoning-effort control for Cursor for that reason
(`providerHonoursEffort`).

---

## 10. When the IDE updates

1. `python3 tools/cursor-schema.py <message>` for each message in §3; diff against the
   table. A moved number is the usual cause of `illegal tag`.
2. Run the probe (§2). If it answers, the transport is fine.
3. If the model has no memory across turns, compare `commit()`'s state against a real
   `composerData.conversationState` (§4) — that is where the schema and the truth have
   diverged before.
4. `go test ./internal/pool/oauth/ -run Cursor` covers the encoders, decoders and both
   terminator rules offline, without touching the network.
