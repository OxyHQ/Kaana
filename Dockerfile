# Kaana's release image.
#
# # Why the build stage is not emulated
#
# Kaana's module requires nothing outside the Go standard library, so there is
# no cgo and nothing to link against a target libc. The build stage therefore
# runs on the BUILDER's architecture and cross-compiles, which is why an arm64
# image can be produced on an amd64 host with no qemu registered at all.
#
# # Why there is no HEALTHCHECK instruction
#
# The final stage has no shell and no binary other than relay itself, so a
# HEALTHCHECK's command could only be one this image does not carry. Adding a
# shell to run it would put a package manager and an interpreter in a container
# whose environment holds provider API keys, to duplicate a check that can be
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
# baking the inventory would freeze its `issuedAt`: past RELAY_INVENTORY_MAX_AGE
# (default 1h) every unpinned model reference would be refused, so the image
# would deploy green and degrade an hour later on a clock nobody was watching.
# Kaana re-reads the file every RELAY_INVENTORY_RELOAD_INTERVAL, so the snapshot
# is mounted at /etc/relay by whatever publishes it. THE RUNTIME PATHS AND THE
# SHIPPED BINARY NAMES STILL SAY `relay` WHILE THE SOURCE SAYS `kaana`, and
# that gap is deliberate: the task definition in oxy-infra names
# `/usr/local/bin/relay-publisher` as its entryPoint and mounts the inventory
# volume at /etc/relay, so moving either here without moving terraform in the
# same breath starts the wrong binary, or mounts the snapshot where nothing
# reads it. They move in the infrastructure rename, not in this one. See README, "Deploying it".

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
    go build -trimpath -ldflags="-s -w" -o /out/relay ./cmd/kaana

# The inventory publisher ships in the SAME image and runs as a different task.
# One image because they are one module built from one commit, and a publisher
# lagging the serving binary would produce snapshots against a contract the
# reader has moved past. What separates them is the task definition: the
# publisher's overrides `entryPoint` to the binary below and runs under a role
# holding `s3:PutObject` on one key, which the serving task's role does not.
#
# Forgetting the override fails loudly rather than silently — the publisher task
# would start `relay`, which refuses to boot without an inventory snapshot.
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /out/relay-publisher ./cmd/kaana-publisher

# The mount point for the configuration snapshot, created here because the
# runtime stage has no shell to mkdir with. A volume mounted over it brings its
# own ownership and shadows this directory entirely, so the chown governs only
# the unmounted case; what the directory is for is to make the mount point part
# of the image's stated contract rather than something a task definition
# invents.
RUN mkdir -p /out/etc/relay && chown -R 65532:65532 /out/etc/relay

# The attribution table is copied into a directory of its OWN, not into
# /etc/relay: that path is a mount point, and a volume mounted over it shadows
# the directory entirely — so a table placed there would vanish on exactly the
# tasks that mount the snapshot.
RUN mkdir -p /out/etc/relay-publisher && cp configs/model-attribution.json /out/etc/relay-publisher/ \
    && chown -R 65532:65532 /out/etc/relay-publisher

# distroless/static carries the CA bundle the provider adapters need for TLS to
# api.openai.com and api.anthropic.com, plus tzdata, and nothing else — no
# shell, no package manager, no libc. The :nonroot tag runs as uid 65532.
FROM gcr.io/distroless/static-debian12:nonroot AS runtime

COPY --from=build /out/relay /usr/local/bin/relay
COPY --from=build /out/relay-publisher /usr/local/bin/relay-publisher
COPY --from=build --chown=65532:65532 /out/etc/relay /etc/relay
COPY --from=build --chown=65532:65532 /out/etc/relay-publisher /etc/relay-publisher

# Where the image reads its configuration snapshot. This is the image's own
# contract with whatever publishes the snapshot, so the task definition does not
# restate it; what the task definition decides is what gets mounted at
# /etc/relay. RELAY_PROVIDER_RATES_PATH is deliberately unset — absent means
# provider cost is not measured, and every measurement says so rather than
# reporting zero.
# THESE ENV DEFAULTS KEEP THE PRE-RENAME SPELLING, AND SWAPPING THEM EARLY WOULD
# INVERT AN OVERRIDE. A container definition's `environment` beats an image ENV,
# which is how oxy-infra sets these. But the binary prefers `KAANA_*` and only
# falls back to `RELAY_*`, so an image setting `KAANA_INVENTORY_PATH` while the
# task definition still sets `RELAY_INVENTORY_PATH` makes the IMAGE default win.
# Today both hold the same value, so it would break nothing and teach nothing —
# which is exactly why it is worth naming. They move to `KAANA_*` in the same
# change as the task definition.
ENV RELAY_INVENTORY_PATH=/etc/relay/inventory.json

# The publisher's attribution table. It carries no secret — it is a public
# statement of who released which weights — so unlike the snapshot it is baked
# in: it changes when a human adds a model, which is a commit, not a cadence.
ENV RELAY_PUBLISHER_ATTRIBUTION_PATH=/etc/relay-publisher/model-attribution.json

# Documentation only, and it matches cmd/kaana's default RELAY_ADDR.
EXPOSE 8080

USER 65532:65532

ENTRYPOINT ["/usr/local/bin/relay"]
