# LiteRouter

LiteRouter is a lightweight AI proxy written in Go. It provides an authenticated OpenAI/Anthropic-compatible gateway, OpenAI/xAI fallback aliases, encrypted OAuth onboarding and quota tracking, context compaction, near-limit summarization, cache primitives, SSE streaming, and an embedded dashboard.

LiteRouter là AI proxy nhẹ viết bằng Go. Hệ thống cung cấp gateway tương thích OpenAI/Anthropic có xác thực, fallback alias OpenAI/xAI, OAuth mã hóa và quota tracker, compact context, summarize gần giới hạn, cache, SSE streaming và dashboard embed.

## Cursor

Cursor is reached over the private protocol its IDE uses — hand-encoded protobuf
against an unpublished schema. When an IDE update breaks it, follow
[docs/cursor-protocol.md](docs/cursor-protocol.md): it records every field number,
how to recover the schema from the IDE bundle (`tools/cursor-schema.py`), and which
explanations have already been measured and ruled out.

## Run with Docker / Chạy bằng Docker

```sh
export LITEROUTER_MASTER_KEY="$(openssl rand -base64 32)"
export LITEROUTER_API_TOKEN="$(openssl rand -hex 32)"
docker compose up -d
curl http://127.0.0.1:8317/health
```

Expected response / Kết quả:

```json
{"status":"ok"}
```

Stop / Dừng:

```sh
docker compose down
```

## Run the binary / Chạy binary

Requirements / Yêu cầu: Go 1.23+.

```sh
cp config.example.yaml config.yaml
export LITEROUTER_MASTER_KEY="$(openssl rand -base64 32)"
export LITEROUTER_API_TOKEN="$(openssl rand -hex 32)"
go run ./cmd/literouter -config config.yaml
```

Without `-config`, LiteRouter uses built-in defaults. Khi không truyền `-config`, LiteRouter dùng cấu hình mặc định.

```sh
go test ./...
go build -trimpath -ldflags="-s -w" -o literouter ./cmd/literouter
./literouter
```

## Proxy API / Proxy API

Set at least one upstream key / Đặt ít nhất một upstream key:

```sh
export OPENAI_API_KEY="..."  # OpenAI models
export XAI_API_KEY="..."     # grok-* models
```

```sh
curl http://127.0.0.1:8317/v1/chat/completions \
  -H "authorization: Bearer $LITEROUTER_API_TOKEN" \
  -H 'content-type: application/json' \
  -d '{"model":"gpt-4.1","messages":[{"role":"user","content":"Hello"}]}'

curl http://127.0.0.1:8317/v1/messages \
  -H "authorization: Bearer $LITEROUTER_API_TOKEN" \
  -H 'content-type: application/json' \
  -d '{"model":"gpt-4.1","max_tokens":256,"messages":[{"role":"user","content":"Hello"}]}'
```

Use `"stream":true` for SSE. Models prefixed `grok-` route to xAI; other models route to OpenAI. Configured aliases appear in `/v1/models`; requests try each concrete model in fallback order before streaming starts. `POST /v1/responses` supports current Codex CLI through the Responses wire protocol; response retrieval, cancellation, compaction, and WebSocket transport are not implemented. OAuth-backed inference dispatch remains deferred.

Dùng `"stream":true` cho SSE. Model có prefix `grok-` đi xAI; model khác đi OpenAI. Alias đã cấu hình xuất hiện ở `/v1/models`; request thử từng model theo thứ tự fallback trước khi stream bắt đầu. `POST /v1/responses` hỗ trợ Codex CLI hiện tại qua Responses protocol; chưa hỗ trợ retrieve, cancel, compact và WebSocket. Dispatch inference qua OAuth vẫn được hoãn.

Open the local dashboard, then unlock mutations with `LITEROUTER_API_TOKEN` / Mở dashboard local rồi unlock thao tác bằng `LITEROUTER_API_TOKEN`: <http://127.0.0.1:8317/ui>

## CLI Tool Setup / Cấu hình CLI

The dashboard generates authenticated Apply/Reset scripts for Claude Code, Codex, and oh-my-pi (`omp`). LiteRouter runs in Docker and never mounts or writes host CLI configuration directly. Select endpoint/models, download the script, then run it on the host; Python 3 is required.

Dashboard tạo script Apply/Reset có xác thực cho Claude Code, Codex và oh-my-pi (`omp`). LiteRouter chạy Docker, không mount hoặc ghi trực tiếp cấu hình CLI trên host. Chọn endpoint/model, tải script rồi chạy trên host; cần Python 3.

```sh
sh ~/Downloads/literouter-claude-apply.sh
sh ~/Downloads/literouter-codex-apply.sh
sh ~/Downloads/literouter-omp-apply.sh
```

Claude uses the base URL without `/v1`; Codex uses `/v1` with `wire_api = "responses"`. Scripts back up and merge existing settings instead of replacing unrelated configuration.

oh-my-pi is configured as a plain provider named `literouter` in `~/.omp/agent/models.yml` (or `$PI_CODING_AGENT_DIR/models.yml`): the base URL (with `/v1`), the API token, and the whole LiteRouter model catalog. Routing — default model, smol/slow/plan roles — is set up inside omp itself, e.g. `omp --model literouter/<id>` or `modelRoles` in omp's `config.yml`.

oh-my-pi được cấu hình như một provider thông thường tên `literouter` trong `~/.omp/agent/models.yml` (hoặc `$PI_CODING_AGENT_DIR/models.yml`): base URL (kèm `/v1`), API token, và toàn bộ catalog model của LiteRouter. Việc điều phối — model mặc định, vai trò smol/slow/plan — được cấu hình ngay trong omp, ví dụ `omp --model literouter/<id>` hoặc `modelRoles` trong `config.yml` của omp.

## OAuth and quota / OAuth và quota

```sh
# Start browser PKCE login: codex or claude
curl -X POST http://127.0.0.1:8317/api/oauth/start \
  -H "authorization: Bearer $LITEROUTER_API_TOKEN" \
  -H 'content-type: application/json' -d '{"provider":"codex"}'

# Grok Build returns a device code and verification URL
curl -X POST http://127.0.0.1:8317/api/oauth/start \
  -H "authorization: Bearer $LITEROUTER_API_TOKEN" \
  -H 'content-type: application/json' -d '{"provider":"grok"}'

# Import local Codex CLI credentials; tokens never appear in the response
curl -X POST http://127.0.0.1:8317/api/oauth/codex/import-cli \
  -H "authorization: Bearer $LITEROUTER_API_TOKEN" \
  -H 'content-type: application/json' -d '{}'

curl http://127.0.0.1:8317/api/accounts \
  -H "authorization: Bearer $LITEROUTER_API_TOKEN"
curl 'http://127.0.0.1:8317/api/accounts/codex:ACCOUNT_ID/quota?refresh=true' \
  -H "authorization: Bearer $LITEROUTER_API_TOKEN"
```

Codex uses loopback port `1455`, with official fallback `1457`. Claude Code uses `1457`. xAI (Grok) uses browser OAuth with loopback callback on `127.0.0.1:1456`. OAuth verifiers and credentials are encrypted at rest. Quota snapshots survive restarts and feed the account pool.

Codex dùng port loopback `1455`, fallback chính thức `1457`. Claude Code dùng `1457`. xAI (Grok) dùng browser OAuth với callback loopback `127.0.0.1:1456`. Verifier và credential được mã hóa; quota snapshot tồn tại qua restart và cấp dữ liệu cho account pool.

## Configuration / Cấu hình

YAML fields are documented in [`config.example.yaml`](config.example.yaml). Environment variables override YAML values.

Các field YAML được ghi chú trong [`config.example.yaml`](config.example.yaml). Biến môi trường ghi đè YAML.

| Environment variable | Default |
|---|---:|
| `LITEROUTER_SERVER_ADDR` | `127.0.0.1:8317` |
| `LITEROUTER_SERVER_READ_TIMEOUT` | `10s` |
| `LITEROUTER_SERVER_WRITE_TIMEOUT` | `0s` (SSE-safe) |
| `LITEROUTER_SERVER_IDLE_TIMEOUT` | `60s` |
| `LITEROUTER_SERVER_SHUTDOWN_TIMEOUT` | `15s` |
| `LITEROUTER_STORAGE_PATH` | `./data/literouter.db` |
| `LITEROUTER_MASTER_KEY` | required / bắt buộc |
| `LITEROUTER_API_TOKEN` | required / bắt buộc |
| `LITEROUTER_OAUTH_REFRESH_INTERVAL` | `1m` |
| `LITEROUTER_ROUTER_STRATEGY` | `smart` |
| `LITEROUTER_CACHE_RESPONSE_TTL` | `15m` |
| `LITEROUTER_CACHE_RESPONSE_MAX_ENTRIES` | `10000` |
| `LITEROUTER_CACHE_COMPRESSION_MODE` | `safe` |
| `LITEROUTER_CACHE_PROMPT_MIN_BYTES` | `4096` |
| `LITEROUTER_CACHE_XAI_PROMPT_CACHE_KEY` | `false` |
| `LITEROUTER_OPENAI_BASE_URL` | `https://api.openai.com/v1` |
| `LITEROUTER_XAI_BASE_URL` | `https://api.x.ai/v1` |
| `LITEROUTER_LOG_LEVEL` | `info` |

`LITEROUTER_MASTER_KEY` must decode to exactly 32 bytes and is never accepted from YAML. Keep the same key after accounts are added; changing it makes stored credentials unreadable.

`LITEROUTER_MASTER_KEY` phải giải mã thành đúng 32 byte và không được nhận từ YAML. Giữ nguyên key sau khi thêm account; đổi key khiến credential đã lưu không thể đọc.

Valid log levels / Log level hợp lệ: `debug`, `info`, `warn`, `error`.

`LITEROUTER_API_TOKEN` protects `/v1/*`, `/api/*`, and UI mutations. Keep the UI on loopback; its read-only dashboard is intentionally accessible without the token.

`LITEROUTER_API_TOKEN` bảo vệ `/v1/*`, `/api/*` và thao tác thay đổi từ UI. Giữ UI trên loopback; dashboard chỉ đọc được mở local không cần token.

## Measured performance / Hiệu năng đo được

Measured on 2026-07-21 with Go 1.24.5 (`darwin/arm64`), Apple M4 Pro. Commands:

```sh
go test ./...
go test -race ./...
go vet ./...
go test -run '^$' -bench=. -benchmem ./internal/gateway ./internal/cache ./internal/contextguard
```

| Benchmark | Time | Memory | Allocations |
|---|---:|---:|---:|
| Gateway non-stream | 2,245 ns/op | 1,472 B/op | 23 allocs/op |
| Warm response cache | 8,371 ns/op | 9,601 B/op | 23 allocs/op |
| Parse 1,000 OpenAI SSE deltas | 640,033 ns/op | 593,649 B/op | 10,994 allocs/op |
| Small tool-result fast path | 4.649 ns/op | 0 B/op | 0 allocs/op |
| Large tool-result compression | 128,202 ns/op | 188,465 B/op | 6 allocs/op |
| Response-key generation | 27,646 ns/op | 37,342 B/op | 75 allocs/op |
| ContextGuard, 100 tool-heavy messages | 446,873 ns/op | 721,158 B/op | 678 allocs/op |

These are absolute microbenchmarks, not direct-provider or 9Router comparisons. p95 proxy overhead, TTFT, inter-chunk jitter, overflow-reduction percentage, Docker image size, and runtime RAM remain unmeasured; no claim is made for those targets.

Đây là microbenchmark tuyệt đối, không phải so sánh với provider trực tiếp hoặc 9Router. p95 proxy overhead, TTFT, inter-chunk jitter, tỷ lệ giảm overflow, kích thước Docker image và RAM runtime chưa được đo; không công bố claim cho các mục tiêu đó.

## Roadmap

OAuth-backed proxy dispatch remains deferred. The current `/v1/*` gateway uses `OPENAI_API_KEY` or `XAI_API_KEY`; OAuth accounts supply onboarding and quota visibility.

OAuth-backed proxy dispatch vẫn được hoãn. Gateway `/v1/*` hiện dùng `OPENAI_API_KEY` hoặc `XAI_API_KEY`; OAuth account phục vụ onboarding và hiển thị quota.
