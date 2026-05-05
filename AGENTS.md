# AGENTS

## Quickstart
- Requires `.env` with `API_BASE` (must include `/v1`), `API_KEY`, `MODEL` (see `.env.example`).
- Build/run: `go build -o llmproxy main.go` then `./llmproxy -host 0.0.0.0 -port 8080 -pricing pricing.json`.
- Client base URL should be `http://localhost:8080` (do not add `/v1`; proxy remaps).

## Runtime behavior
- SQLite log DB is `./tracer.db` and is created/used automatically.
- Dashboard and APIs: `/dashboard`, `/api/logs`, `/api/logs/{id}`, `/api/logs/last-updated`, `/api/stats`, `/api/config` on the proxy host.
- `/api/logs` supports pagination via `page` and `page_size` (max 200) and omits request/response bodies; `/api/logs/{id}` returns full bodies.
- `/api/logs/last-updated` returns the latest log id/timestamp for lightweight refresh checks.
- Pricing is loaded from `pricing.json` (per-1M-token, model-prefix matching in code).
- Proxy ignores `/.well-known/appspecific/com.chrome.devtools.json` (returns 404).

## Repo structure
- Single Go module (`go.mod`); main entrypoint is `main.go`.
- User systemd service template: `llmproxy.service`.

## CI / automation
- No `.github/workflows` directory in this repo.
