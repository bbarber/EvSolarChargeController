# The ZeroMQ binding is cgo, so this cannot be a static scratch image: libzmq has to be present at
# build time and again at runtime. That is the price of the only fleet-telemetry dispatcher that
# needs no broker — the alternatives (Kafka, Kinesis, Pub/Sub, MQTT, Redis) would each mean another
# container on a 1 OCPU box.

FROM golang:1.26-bookworm AS build

RUN apt-get update \
 && apt-get install -y --no-install-recommends libzmq3-dev pkg-config \
 && rm -rf /var/lib/apt/lists/*

WORKDIR /src

# Copied first so the dependency layer survives source edits.
COPY go.mod go.sum ./
RUN go mod download

COPY cmd/ ./cmd/
COPY internal/ ./internal/

RUN CGO_ENABLED=1 go build -trimpath -ldflags="-s -w" -o /out/evsolar ./cmd/evsolar
# Registration has to run where the database is: both providers rotate their refresh token on use,
# so running it anywhere else would consume the token the controller is holding.
RUN CGO_ENABLED=1 go build -trimpath -ldflags="-s -w" -o /out/evsolar-register ./cmd/evsolar-register

FROM debian:bookworm-slim

RUN apt-get update \
 && apt-get install -y --no-install-recommends libzmq5 ca-certificates tzdata \
 && rm -rf /var/lib/apt/lists/*

# The database holds live refresh tokens, so nothing here runs as root.
RUN useradd --system --uid 10001 --home /var/lib/evsolar --create-home evsolar

COPY --from=build /out/evsolar /usr/local/bin/evsolar
COPY --from=build /out/evsolar-register /usr/local/bin/evsolar-register

USER evsolar
WORKDIR /var/lib/evsolar

ENTRYPOINT ["/usr/local/bin/evsolar"]
