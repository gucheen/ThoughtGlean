# syntax=docker/dockerfile:1

FROM node:22-alpine AS web-build

WORKDIR /src
COPY package.json package-lock.json ./
RUN npm ci

COPY tsconfig.json tsconfig.app.json tsconfig.node.json vite.config.ts ./
COPY frontend ./frontend
RUN npm run build


FROM golang:1.26-bookworm AS relay-build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY cmd ./cmd
COPY internal ./internal
COPY --from=web-build /src/internal/webui/assets ./internal/webui/assets
RUN CGO_ENABLED=1 go build -trimpath -ldflags="-s -w" -o /out/thoughtglean ./cmd/thoughtglean


FROM debian:bookworm-slim AS relay

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd --gid 10001 thoughtglean \
    && useradd --uid 10001 --gid thoughtglean --no-create-home --shell /usr/sbin/nologin thoughtglean \
    && mkdir -p /data \
    && chown thoughtglean:thoughtglean /data

COPY --from=relay-build /out/thoughtglean /app/thoughtglean

ENV THOUGHTGLEAN_ADDR=:8080
ENV THOUGHTGLEAN_DATA_DIR=/data

USER thoughtglean
VOLUME ["/data"]
EXPOSE 8080

HEALTHCHECK --interval=15s --timeout=3s --start-period=5s --retries=5 \
  CMD curl --fail --silent http://127.0.0.1:8080/api/health >/dev/null || exit 1

ENTRYPOINT ["/app/thoughtglean"]


FROM nginx:1.27-alpine AS web

COPY docker/nginx.conf /etc/nginx/nginx.conf
COPY --from=web-build /src/internal/webui/assets /usr/share/nginx/html

EXPOSE 8080

HEALTHCHECK --interval=15s --timeout=3s --start-period=5s --retries=5 \
  CMD wget -q -O /dev/null http://127.0.0.1:8080/ || exit 1
