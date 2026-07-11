# Local DATABASE_URL / MQTT_BROKER for the compose services (see docker-compose.yml).
LOCAL_DB_URL ?= postgres://ops:ops@localhost:5433/ops?sslmode=disable
LOCAL_MQTT_BROKER ?= tcp://localhost:1884

.PHONY: db-up db-down migrate test integration

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
