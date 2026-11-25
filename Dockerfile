# idlerthing — self-hosted server/service inventory, as a container image.
#
# Two stages so the runtime image carries only the binary + CA roots:
#   builder — compile the CGO-free static binary (web assets, migrations and
#             the AGPL notices are all go:embed'ed, so the binary is the app)
#   final   — minimal Alpine runtime, non-root, HTTP only (TLS at the proxy)
#
# Build (stamps the version the same way `make build` does):
#   make docker
# or by hand:
#   docker build -t idlerthing:local --build-arg VERSION=$(git describe --tags --always --dirty) .
#
# Published multi-arch to ghcr.io/alteredparadox/idlerthing by the release
# workflow on every version tag. See deploy/docker/ for a compose file.

# ---- builder: compile the static binary ----
# Runs on the BUILD platform and cross-compiles to the target arch. modernc
# SQLite is pure Go and CGO is off, so cross-compiling is just a GOARCH
# switch — no QEMU emulation of the (slow) Go compile for arm64.
FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS builder
# The official golang images set GOTOOLCHAIN=local, which silently ignores
# go.mod's `toolchain` line — the minimum patch level a release may be built
# with (stdlib CVE fixes ship in patch releases). auto honours it: if this
# base image ever lags behind that line, go fetches the required toolchain
# instead of building a binary with known-vulnerable stdlib.
ENV GOTOOLCHAIN=auto
WORKDIR /src
# Module download is its own cached layer keyed on go.mod/go.sum only.
COPY go.mod go.sum ./
RUN go mod download
# Copy only what the build compiles or embeds — explicit paths (rather than a
# recursive `COPY . .`) keep local databases, secrets and reference material
# out of the build. Embedded: LICENSE + THIRD_PARTY_LICENSES.md (notices.go),
# internal/db/migrations, internal/web/assets.
COPY main.go notices.go LICENSE THIRD_PARTY_LICENSES.md ./
COPY internal/ ./internal/
# TARGETOS/TARGETARCH are set by buildx (and default to the host on a plain
# `docker build`). VERSION is what `idlerthing version` reports, and /source
# pins its AGPL offer to /tree/<VERSION> for anything but "dev" — so an
# unstamped build must stay "dev" rather than claim a tag that doesn't exist.
ARG TARGETOS TARGETARCH VERSION=dev
RUN CGO_ENABLED=0 GOOS="${TARGETOS}" GOARCH="${TARGETARCH}" go build -trimpath \
      -ldflags="-s -w -X main.version=${VERSION}" \
      -o /out/idlerthing .

# ---- final: minimal runtime ----
# Track a current, supported Alpine branch; the minor tag picks up patch
# releases with CVE fixes on every rebuild.
FROM alpine:3.24
# ca-certificates: outbound TLS (Prometheus scrapes, exchange rates, whois,
# YABS links) fails without the roots. tzdata: correct local-time rendering.
# busybox (already in the base) provides /bin/ping for the in-app ping tool
# and wget for the HEALTHCHECK.
RUN apk add --no-cache ca-certificates tzdata \
 && addgroup -g 10001 -S idlerthing \
 && adduser -u 10001 -S -G idlerthing -h /var/lib/idlerthing idlerthing \
 && mkdir -p /var/lib/idlerthing \
 && chown idlerthing:idlerthing /var/lib/idlerthing \
 && chmod 0700 /var/lib/idlerthing
COPY --from=builder /out/idlerthing /usr/local/bin/idlerthing
USER idlerthing
WORKDIR /var/lib/idlerthing
# Container defaults. The binary's own default (127.0.0.1) would be
# unreachable through a published port. The DB lands where the systemd unit
# puts it, and the generated YABS secret (<db>.secret) sits beside it — both
# on the volume, so they survive image upgrades. GOMEMLIMIT matches the unit.
ENV IDLER_ADDR=0.0.0.0:8080 \
    IDLER_DB=/var/lib/idlerthing/data/idlerthing.db \
    GOMEMLIMIT=64MiB
VOLUME ["/var/lib/idlerthing"]
EXPOSE 8080
# /healthz is unauthenticated and pings the database, so it proves both the
# HTTP server and the SQLite file are usable.
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD wget -q -O /dev/null http://127.0.0.1:8080/healthz || exit 1
# No CMD: arguments go straight to the binary, so `docker run IMAGE version`
# and `docker compose exec idlerthing idlerthing passwd` both work.
ENTRYPOINT ["/usr/local/bin/idlerthing"]
