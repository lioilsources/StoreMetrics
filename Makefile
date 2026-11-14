BIN    := bin/store-metrics
INGEST := bin/telemetry-ingest
HOST   ?= joda
DIR    := deploy/store-metrics
GOBUILD := GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags="-s -w"

build:
	$(GOBUILD) -o $(BIN) ./cmd/store-metrics
	$(GOBUILD) -o $(INGEST) ./cmd/telemetry-ingest

test:
	go vet ./...
	go test ./...

# Copies binaries, config and units. Never secrets or .env: those are placed once by hand (see README).
# The telemetry services are only restarted when they already run (enabled via COMPOSE_PROFILES=telemetry);
# --no-deps keeps the database container untouched.
deploy: test build
	ssh $(HOST) 'mkdir -p ~/$(DIR)/bin ~/$(DIR)/raw ~/$(DIR)/export ~/$(DIR)/secrets ~/.config/systemd/user && chmod 700 ~/$(DIR)/secrets'
	rsync -a $(BIN) $(INGEST) $(HOST):~/$(DIR)/bin/
	rsync -a apps.yaml deploy/docker-compose.yml deploy/Caddyfile $(HOST):~/$(DIR)/
	rsync -a deploy/store-metrics.service deploy/store-metrics.timer $(HOST):~/.config/systemd/user/
	ssh $(HOST) 'systemctl --user daemon-reload && systemctl --user enable --now store-metrics.timer'
	ssh $(HOST) 'cd ~/$(DIR) && if [ -n "$$(docker compose ps -q ingest 2>/dev/null)" ]; then docker compose up -d --no-deps ingest caddy cloudflared && docker compose restart ingest caddy; fi'
	ssh $(HOST) "echo '{\"ts\":\"'\$$(date -u +%FT%TZ)'\",\"who\":\"$(USER)@M2\",\"repo\":\"store-metrics\",\"action\":\"deploy\"}' >> ~/deploy/events.jsonl"

.PHONY: build test deploy
