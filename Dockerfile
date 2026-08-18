# Relay's release image.
#
# # Why the build stage is not emulated
#
# Relay's module requires nothing outside the Go standard library, so there is
# no cgo and nothing to link against a target libc. The build stage therefore
# runs on the BUILDER's architecture and cross-compiles, which is why an arm64
# image can be produced on an amd64 host with no qemu registered at all.
#
# # Why there is no HEALTHCHECK instruction
#
# The final stage has no shell and no binary other than relay itself, so a
# HEALTHCHECK's command could only be one this image does not carry. Adding a
# shell to run it would put a package manager and an interpreter in a container
# whose environment holds provider API keys, to duplicate a check the platform
# already makes: ECS does not read a Dockerfile HEALTHCHECK, and the target
# group polls the process over HTTP instead.
#
#     GET /livez   unsigned, carries no provider detail, 200 with
#                  {"status":"ok","contractVersion":"..."}
#
# It is the only route that answers without an Oxy edge signature, so it is the
# only one a load balancer can use. `GET /internal/v1/health` is the operator
# surface and is signed.
#
# # What this image deliberately does not contain
#
# No inventory snapshot and no provider rate card. Both are business data, and
# baking the inventory would freeze its `issuedAt`: past RELAY_INVENTORY_MAX_AGE
# (default 1h) every unpinned model reference would be refused, so the image
# would deploy green and degrade an hour later on a clock nobody was watching.
# Relay re-reads the file every RELAY_INVENTORY_RELOAD_INTERVAL, so the snapshot
# is mounted at /etc/relay by whatever publishes it. See README, "Deploying it".

ARG GO_VERSION=1.24.4

FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-bookworm AS build

WORKDIR /src

# .dockerignore is the allow-list: it names the only paths that may enter the
# build context, so this copies the whole context rather than restating that
# list in a second place where the two could disagree.
COPY . .

# Declared without defaults because BuildKit populates them from --platform, and
# from the HOST when no platform is given — measured: an unqualified build on an
# amd64 machine produces an amd64 image from this file. So the target is a build
# input with no safe default to fall back to, and the deploy workflow states
# `--platform linux/arm64` rather than inheriting whichever runner it landed on.
ARG TARGETOS
ARG TARGETARCH

# CGO_ENABLED=0 because the runtime stage has no libc to dynamically link
# against. -trimpath keeps the builder's directory layout out of the binary,
# and -s -w drop the symbol table and DWARF sections, which a panic in a Go
# binary does not need: the runtime's own traceback is built from the pclntab
# and survives both.
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /out/relay ./cmd/relay

# The mount point for the configuration snapshot, created here because the
# runtime stage has no shell to mkdir with. A volume mounted over it brings its
# own ownership and shadows this directory entirely, so the chown governs only
# the unmounted case; what the directory is for is to make the mount point part
# of the image's stated contract rather than something a task definition
# invents.
RUN mkdir -p /out/etc/relay && chown -R 65532:65532 /out/etc/relay

# distroless/static carries the CA bundle the provider adapters need for TLS to
# api.openai.com and api.anthropic.com, plus tzdata, and nothing else — no
# shell, no package manager, no libc. The :nonroot tag runs as uid 65532.
FROM gcr.io/distroless/static-debian12:nonroot AS runtime

COPY --from=build /out/relay /usr/local/bin/relay
COPY --from=build --chown=65532:65532 /out/etc/relay /etc/relay

# Where the image reads its configuration snapshot. This is the image's own
# contract with whatever publishes the snapshot, so the task definition does not
# restate it; what the task definition decides is what gets mounted at
# /etc/relay. RELAY_PROVIDER_RATES_PATH is deliberately unset — absent means
# provider cost is not measured, and every measurement says so rather than
# reporting zero.
ENV RELAY_INVENTORY_PATH=/etc/relay/inventory.json

# Documentation only, and it matches cmd/relay's default RELAY_ADDR.
EXPOSE 8080

USER 65532:65532

ENTRYPOINT ["/usr/local/bin/relay"]
