# Multi-binary image: every connector, the dashboard, the account tool and the
# migrate tool share one layer. The workload picks the entrypoint
# (command: [/usr/local/bin/<name>]) — building separate images off one Go module
# would only multiply pull weight and drift.
FROM golang:1.25 AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
# Static: the runtime layer has no libc.
# google-auth ships too: onboarding an app-password mailbox needs an IMAP login
# from somewhere that can reach both the mail server and the ops db, and a
# one-shot Job on this image is that place — no workstation in the loop.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" \
      -o /out/ ./cmd/connectors/... ./cmd/tools/migrate ./cmd/dashboard ./cmd/google-auth

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/ /usr/local/bin/
# Shipped so a migrate Job can apply schema from the same image tag that runs it.
COPY --from=build /src/migrations /migrations
USER nonroot
