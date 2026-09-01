# Kaana's release image.
#
# # Why the build stage is not emulated
#
# Kaana's dependencies are pure Go, so there is no cgo and nothing to link
# against a target libc. The build stage therefore
# runs on the BUILDER's architecture and cross-compiles, which is why an arm64
# image can be produced on an amd64 host with no qemu registered at all.
#
# # Why there is no HEALTHCHECK instruction
#
# The final stage has no shell and no binary other than kaana itself, so a
# HEALTHCHECK's command could only be one this image does not carry. Adding a
# shell to run it would put a package manager and an interpreter in a container
# that decrypts provider keys in memory, to duplicate a check that can be
# made from outside.
#
# The consequence belongs with the deployment and not only here: an ECS
# container `healthCheck` cannot work against this image either, for the same
# reason and with the same three missing binaries, and it fails by never
# passing rather than by erroring. Whatever watches this process has to reach
# it over HTTP from outside the container.
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
# baking the inventory would freeze its `issuedAt`: past KAANA_INVENTORY_MAX_AGE
# (default 1h) every unpinned model reference would be refused, so the image
# would deploy green and degrade an hour later on a clock nobody was watching.
# Kaana re-reads the file every KAANA_INVENTORY_RELOAD_INTERVAL, so the snapshot
# is mounted at /etc/kaana by whatever publishes it. The task definition in
# oxy-infra names `/usr/local/bin/kaana-publisher` as its entryPoint and mounts
# the inventory volume at that same path.

ARG GO_VERSION=1.26.7

FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-bookworm@sha256:e8c859f5632dcfde7b32d2012b4351728f6437930887c2f6a91ea242459e5514 AS build

WORKDIR /src

# PostgreSQL uses the RDS trust store rather than assuming a general web-PKI
# bundle contains the regional database CA. The checksum makes an upstream
# replacement a reviewed source change instead of mutable build input.
ADD --checksum=sha256:e5bb2084ccf45087bda1c9bffdea0eb15ee67f0b91646106e466714f9de3c7e3 \
    https://truststore.pki.rds.amazonaws.com/global/global-bundle.pem \
    /tmp/aws-rds-global-bundle.pem

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

# Keep cross-compilation bounded on ARM64 builders. This affects only the build
# stage, not Kaana's runtime workers.
ENV GOMAXPROCS=2
ENV GOFLAGS=-p=2

# CGO_ENABLED=0 because the runtime stage has no libc to dynamically link
# against. -trimpath keeps the builder's directory layout out of the binary,
# and -s -w drop the symbol table and DWARF sections, which a panic in a Go
# binary does not need: the runtime's own traceback is built from the pclntab
# and survives both.
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /out/kaana ./cmd/kaana

# The inventory publisher ships in the SAME image and runs as a different task.
# One image because they are one module built from one commit, and a publisher
# lagging the serving binary would produce snapshots against a contract the
# reader has moved past. What separates them is the task definition: the
# publisher's overrides `entryPoint` to the binary below and runs under a role
# holding `s3:PutObject` on one key, which the serving task's role does not.
#
# Forgetting the override fails loudly rather than silently — the publisher task
# would start `kaana`, which refuses to boot without an inventory snapshot.
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /out/kaana-publisher ./cmd/kaana-publisher

# Operators run migrations and key lifecycle commands as one-shot tasks. The
# binary accepts provider plaintext only on stdin and never through argv or env.
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /out/kaana-credentials ./cmd/kaana-credentials

# The mount point for the configuration snapshot, created here because the
# runtime stage has no shell to mkdir with. A volume mounted over it brings its
# own ownership and shadows this directory entirely, so the chown governs only
# the unmounted case; what the directory is for is to make the mount point part
# of the image's stated contract rather than something a task definition
# invents.
RUN mkdir -p /out/etc/kaana && chown -R 65532:65532 /out/etc/kaana

# The attribution table is copied into a directory of its OWN, not into
# /etc/kaana: that path is a mount point, and a volume mounted over it shadows
# the directory entirely — so a table placed there would vanish on exactly the
# tasks that mount the snapshot.
RUN mkdir -p /out/etc/kaana-publisher && cp configs/model-attribution.json /out/etc/kaana-publisher/ \
    && chown -R 65532:65532 /out/etc/kaana-publisher

RUN mkdir -p /out/etc/ssl/certs \
    && cp /tmp/aws-rds-global-bundle.pem /out/etc/ssl/certs/aws-rds-global-bundle.pem \
    && chown 65532:65532 /out/etc/ssl/certs/aws-rds-global-bundle.pem

# distroless/static carries the CA bundle the provider adapters need for TLS to
# api.openai.com and api.anthropic.com, plus tzdata, and nothing else — no
# shell, no package manager, no libc. The :nonroot tag runs as uid 65532.
FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab AS runtime

COPY --from=build /out/kaana /usr/local/bin/kaana
COPY --from=build /out/kaana-publisher /usr/local/bin/kaana-publisher
COPY --from=build /out/kaana-credentials /usr/local/bin/kaana-credentials
COPY --from=build --chown=65532:65532 /out/etc/kaana /etc/kaana
COPY --from=build --chown=65532:65532 /out/etc/kaana-publisher /etc/kaana-publisher
COPY --from=build --chown=65532:65532 /out/etc/ssl/certs/aws-rds-global-bundle.pem /etc/ssl/certs/aws-rds-global-bundle.pem

# Where the image reads its configuration snapshot. This is the image's own
# contract with whatever publishes the snapshot, so the task definition does not
# restate it; what the task definition decides is what gets mounted at
# /etc/kaana. KAANA_PROVIDER_RATES_PATH is deliberately unset — absent means
# provider cost is not measured, and every measurement says so rather than
# reporting zero.
# The task definition restates this value because container environment wins
# over image defaults. Both declarations therefore move together.
ENV KAANA_INVENTORY_PATH=/etc/kaana/inventory.json

# The publisher's attribution table. It carries no secret — it is a public
# statement of who released which weights — so unlike the snapshot it is baked
# in: it changes when a human adds a model, which is a commit, not a cadence.
ENV KAANA_PUBLISHER_ATTRIBUTION_PATH=/etc/kaana-publisher/model-attribution.json

# Documentation only, and it matches cmd/kaana's default KAANA_ADDR.
EXPOSE 8080

USER 65532:65532

ENTRYPOINT ["/usr/local/bin/kaana"]
