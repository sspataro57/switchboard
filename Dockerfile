# Multi-binary image: every connector plus the migrate tool share one layer.
# The CronJob picks the entrypoint (command: [/usr/local/bin/<name>]) — building
# five images off one Go module would only multiply pull weight and drift.
FROM golang:1.25 AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
# Static: the runtime layer has no libc.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" \
      -o /out/ ./cmd/connectors/... ./cmd/tools/migrate

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/ /usr/local/bin/
# Shipped so a migrate Job can apply schema from the same image tag that runs it.
COPY --from=build /src/migrations /migrations
USER nonroot
