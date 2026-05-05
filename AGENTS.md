# AGENTS

## Quickstart
- Requires `.env` with `API_BASE` (must include `/v1`), `API_KEY`, `MODEL` (see `.env.example`).
- Build/run: `go build -o llmproxy main.go` then `./llmproxy -host 0.0.0.0 -port 8080 -pricing pricing.json`.
- Client base URL should be `http://localhost:8080` (do not add `/v1`; proxy remaps).

## Runtime behavior
- SQLite log DB is `./tracer.db` and is created/used automatically.
- Dashboard and APIs: `/dashboard`, `/api/logs`, `/api/stats` on the proxy host.
- Pricing is loaded from `pricing.json` (per-1M-token, model-prefix matching in code).

## Repo structure
- Single Go module (`go.mod`); main entrypoint is `main.go`.
- User systemd service template: `llmproxy.service`.

## CI / automation
- No `.github/workflows` directory in this repo.
