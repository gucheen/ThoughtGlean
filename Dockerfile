FROM golang:1.26-bookworm AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=1 go build -trimpath -ldflags="-s -w" -o /out/thoughtglean ./cmd/thoughtglean

FROM debian:bookworm-slim

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/*

COPY --from=build /out/thoughtglean /app/thoughtglean

ENV THOUGHTGLEAN_ADDR=:8080
ENV THOUGHTGLEAN_DATA_DIR=/data

VOLUME ["/data"]
EXPOSE 8080

ENTRYPOINT ["/app/thoughtglean"]
