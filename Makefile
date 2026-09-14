# Local DATABASE_URL / MQTT_BROKER for the compose services (see docker-compose.yml).
LOCAL_DB_URL ?= postgres://ops:ops@localhost:5433/ops?sslmode=disable
LOCAL_MQTT_BROKER ?= tcp://localhost:1884

.PHONY: db-up db-down migrate test integration install-skill

db-up:
	docker compose up -d --wait

db-down:
	docker compose down -v

migrate:
	DATABASE_URL=$(LOCAL_DB_URL) go run ./cmd/tools/migrate --dir migrations

test:
	go test ./...

# -p 1 serializes test packages: integration suites share one Postgres and a
# global triage filter — concurrent packages would cross-pollute.
integration: db-up
	DATABASE_URL=$(LOCAL_DB_URL) go run ./cmd/tools/migrate --dir migrations
	DATABASE_URL=$(LOCAL_DB_URL) MQTT_BROKER=$(LOCAL_MQTT_BROKER) go test -tags integration -p 1 -count=1 ./...

# SWT-52: install the swb-status skill at Claude Code USER scope. Run it on
# main: it COPIES the checked-out file (never a symlink, which would serve
# whatever branch is checked out to every session). Re-run after any merge that
# touches skills/swb-status/. Same line as docs/runbooks/ops-mcp-user-scope.md.
install-skill:
	install -D -m 0644 skills/swb-status/SKILL.md ~/.claude/skills/swb-status/SKILL.md
