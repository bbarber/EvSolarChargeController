# Tesla vehicle-command HTTP proxy, packaged for Azure Container Apps.
#
# Why this exists: vehicles built after 2021 reject unsigned commands, so set_charging_amps cannot
# be POSTed straight to Fleet API. tesla-http-proxy signs each command with the application's
# private key before forwarding it.
#
# Why nginx is in front: tesla-http-proxy mandates TLS and offers no way to disable it, while
# Container Apps' HTTP ingress speaks plain HTTP to the container. nginx accepts that plain HTTP on
# 8080 and re-wraps it as HTTPS to the proxy on loopback:4443. Traffic is still encrypted
# everywhere it leaves the container — Container Apps terminates a real certificate at the edge.

# vehicle-command's go.mod requires go >= 1.23.
FROM golang:1.23-bookworm AS build

ARG VEHICLE_COMMAND_VERSION=v0.4.1

RUN go install github.com/teslamotors/vehicle-command/cmd/tesla-http-proxy@${VEHICLE_COMMAND_VERSION}

FROM debian:bookworm-slim

RUN apt-get update \
 && apt-get install -y --no-install-recommends nginx openssl ca-certificates gettext-base \
 && rm -rf /var/lib/apt/lists/*

COPY --from=build /go/bin/tesla-http-proxy /usr/local/bin/tesla-http-proxy
COPY tools/tesla-command-proxy/nginx.conf.template /etc/nginx/templates/proxy.conf.template
COPY tools/tesla-command-proxy/entrypoint.sh /usr/local/bin/entrypoint.sh

RUN chmod +x /usr/local/bin/entrypoint.sh

# 8080 is what Container Apps ingress targets; 4443 stays on loopback.
EXPOSE 8080

ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
