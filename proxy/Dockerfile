# syntax=docker/dockerfile:1

# ── build stage ───────────────────────────────────────
FROM golang:1.24-alpine AS build
WORKDIR /src

# Layer-cache the module files first.
COPY go.mod ./
RUN go mod download

COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build \
      -trimpath -ldflags="-s -w -X main.buildVersion=${VERSION}" \
      -o /out/commandcode-proxy ./cmd/commandcode-proxy

# ── runtime stage ─────────────────────────────────────
FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app

COPY --from=build /out/commandcode-proxy /app/commandcode-proxy

# Distroless nonroot already runs as uid 65532.
EXPOSE 3050
ENTRYPOINT ["/app/commandcode-proxy"]
