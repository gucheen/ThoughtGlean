# syntax=docker/dockerfile:1

FROM node:22-alpine AS web-build

WORKDIR /src
ENV PNPM_HOME=/pnpm
ENV PATH=$PNPM_HOME:$PATH
RUN corepack enable

COPY package.json pnpm-lock.yaml pnpm-workspace.yaml ./
RUN pnpm install --frozen-lockfile

COPY tsconfig.json tsconfig.app.json tsconfig.node.json vite.config.ts ./
COPY frontend ./frontend
RUN pnpm build


FROM golang:1.26-bookworm AS server-build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=1 go build -trimpath -ldflags="-s -w" -o /out/thoughtglean ./cmd/thoughtglean


FROM debian:bookworm-slim AS server

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl gosu \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd --gid 10001 thoughtglean \
    && useradd --uid 10001 --gid thoughtglean --no-create-home --shell /usr/sbin/nologin thoughtglean \
    && mkdir -p /data \
    && chown thoughtglean:thoughtglean /data

COPY --from=server-build /out/thoughtglean /app/thoughtglean
COPY docker/server-entrypoint.sh /app/server-entrypoint.sh
RUN chmod 0755 /app/server-entrypoint.sh

ENV THOUGHTGLEAN_ADDR=:8080
ENV THOUGHTGLEAN_DATA_DIR=/data

VOLUME ["/data"]
EXPOSE 8080

HEALTHCHECK --interval=15s --timeout=3s --start-period=5s --retries=5 \
  CMD curl --fail --silent http://127.0.0.1:8080/api/health >/dev/null || exit 1

ENTRYPOINT ["/app/server-entrypoint.sh"]


FROM nginx:1.27-alpine AS web

COPY docker/nginx.conf /etc/nginx/nginx.conf
COPY --from=web-build /src/internal/webui/assets /usr/share/nginx/html

EXPOSE 8080

HEALTHCHECK --interval=15s --timeout=3s --start-period=5s --retries=5 \
  CMD wget -q -O /dev/null http://127.0.0.1:8080/ || exit 1
