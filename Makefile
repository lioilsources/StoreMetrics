BIN  := bin/store-metrics
HOST ?= joda
DIR  := deploy/store-metrics

build:
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o $(BIN) ./cmd/store-metrics

test:
	go test ./...

# Copies binary, config and units. Never secrets or .env: those are placed once by hand (see README).
deploy: test build
	ssh $(HOST) 'mkdir -p ~/$(DIR)/bin ~/$(DIR)/raw ~/$(DIR)/export ~/$(DIR)/secrets ~/.config/systemd/user && chmod 700 ~/$(DIR)/secrets'
	rsync -a $(BIN) $(HOST):~/$(DIR)/bin/
	rsync -a apps.yaml deploy/docker-compose.yml $(HOST):~/$(DIR)/
	rsync -a deploy/store-metrics.service deploy/store-metrics.timer $(HOST):~/.config/systemd/user/
	ssh $(HOST) 'systemctl --user daemon-reload && systemctl --user enable --now store-metrics.timer'
	ssh $(HOST) "echo '{\"ts\":\"'\$$(date -u +%FT%TZ)'\",\"who\":\"$(USER)@M2\",\"repo\":\"store-metrics\",\"action\":\"deploy\"}' >> ~/deploy/events.jsonl"

.PHONY: build test deploy
