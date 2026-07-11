# Local DATABASE_URL for the compose Postgres (see docker-compose.yml).
LOCAL_DB_URL ?= postgres://ops:ops@localhost:5433/ops?sslmode=disable

.PHONY: db-up db-down migrate test integration

db-up:
	docker compose up -d --wait

db-down:
	docker compose down -v

migrate:
	DATABASE_URL=$(LOCAL_DB_URL) go run ./cmd/tools/migrate --dir migrations

test:
	go test ./...

integration: db-up
	DATABASE_URL=$(LOCAL_DB_URL) go run ./cmd/tools/migrate --dir migrations
	DATABASE_URL=$(LOCAL_DB_URL) go test -tags integration ./...
