# syntax=docker/dockerfile:1
# ReFlux egress: the exit nodes' only way out. Two AmneziaWG tunnels (on
# the host's amneziawg kernel module), RU prefix routing, a kill switch and
# a DNS-over-TLS resolver, run by cmd/reflux-egress.
# Build from the repository root:
#   docker build -f deploy/reflux/egress/Dockerfile .

# amneziawg-tools, pinned by commit: v3.1.20260812, the version the host's
# kernel module ships with (AWG 2.0: S3/S4, H ranges, I1-I5).
FROM alpine:3.22 AS awg
ARG AWG_TOOLS_COMMIT=ee0f0a9aa34ff0a0da4b3433b9512781cfe02843
RUN apk add --no-cache build-base linux-headers git
RUN git clone https://github.com/amnezia-vpn/amneziawg-tools.git /src \
 && git -C /src checkout --detach "$AWG_TOOLS_COMMIT" \
 && test "$(git -C /src rev-parse HEAD)" = "$AWG_TOOLS_COMMIT" \
 && make -C /src/src wg \
 && install -m 0755 /src/src/wg /out-awg

FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
# check-docs opens mail.ru documents with the core's transport.
COPY transport ./transport
COPY utils ./utils
COPY netbind ./netbind
COPY cmd/reflux-egress ./cmd/reflux-egress
RUN --mount=type=cache,target=/root/.cache/go-build --mount=type=cache,target=/go/pkg/mod \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/reflux-egress ./cmd/reflux-egress
# A snapshot of the Russian IPv4 space, so the egress routes Russia from
# its first second; it refreshes the list daily at run time.
ADD https://ftp.ripe.net/pub/stats/ripencc/delegated-ripencc-latest /tmp/delegated
RUN /out/reflux-egress ru-cidr < /tmp/delegated > /out/ru.cidr

FROM alpine:3.22
# iproute2: `ip -batch` and `type amneziawg` (busybox ip has neither).
# unbound + dnssec-root: the validating DNS-over-TLS resolver.
RUN apk add --no-cache iproute2 nftables unbound dnssec-root ca-certificates
COPY --from=awg /out-awg /usr/bin/awg
COPY --from=build /out/reflux-egress /usr/local/bin/reflux-egress
COPY --from=build /out/ru.cidr /usr/share/reflux-egress/ru.cidr
HEALTHCHECK --interval=15s --timeout=5s --start-period=60s CMD ["reflux-egress", "health"]
ENTRYPOINT ["reflux-egress"]
CMD ["run"]
