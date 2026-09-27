# The container image (whitepaper §8.3).
#
# Two things shape it. First, there is nothing in the runtime layer but the two
# programs and a set of CA certificates — no shell, no package manager, nothing
# for an intruder to pick up. Second, the container takes exactly the same
# credential path as a host install (§8.3, 1.3): nothing to prepare; you sign in,
# and the daemon writes credentials.key into /data.
#
# /data must be writable: signing in writes credentials.key, and every token
# rotation writes it again. No credential file is in the image. .dockerignore
# keeps them out of the build context, and CI asserts it on every change.
#
# Build both architectures with:
#   docker buildx build --platform linux/amd64,linux/arm64 \
#     -f deploy/docker/Dockerfile -t <registry>/opendrive-bridge:<version> --push .

# The Go version here is not a preference, it is the standard library that ends up
# inside the shipped binary. This was golang:1.24-alpine while everything else in
# the project built with the current toolchain, and trivy's first run found twelve
# HIGH findings in stdlib v1.24.13 — all of them fixed months earlier in 1.25 and
# 1.26. Nothing was wrong with our code; the container was simply built by an old
# compiler and nobody had looked.
#
# govulncheck did not object, and was right not to: it reports vulnerabilities the
# code actually reaches, using the toolchain running it. trivy reads the version
# stamped into the binary and reports everything known about it. Both answers are
# correct, which is why the release gate needs both.
#
# Deliberately unpinned, to match the policy the rest of the project already
# follows: the six released binaries are built with `go-version: stable`, so a
# pinned container was guaranteed to drift older than them, which is precisely
# how this happened. Pinning the minor would have been the same bug with a
# shorter fuse — and it could not be checked here anyway, since the newest 1.26
# image is not necessarily the patch release a given CVE was fixed in.
#
# The risk of floating is a toolchain bump breaking the build; Go's compatibility
# promise makes that unlikely, and CI would hit it at the same moment through
# `stable`. trivy now scans the image on every change, so an old standard library
# is caught by whatever change is in flight rather than at release time.
FROM --platform=$BUILDPLATFORM golang:alpine AS builder

# Cross-compilation targets come from buildx.
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ARG COMMIT=none
ARG BUILD_DATE=unknown

WORKDIR /src

# Dependencies first, so a source-only change does not re-download them.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath \
      -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.buildDate=${BUILD_DATE}" \
      -o /out/opendrived ./cmd/opendrived \
 && CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath \
      -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.buildDate=${BUILD_DATE}" \
      -o /out/odctl ./cmd/odctl \
 && mkdir -p /out/data/cache /out/data/jobs

# distroless/static carries CA certificates, /etc/passwd and nothing else, and runs
# as root — the ordinary container arrangement, and since 1.3 on purpose. Until
# 1.2 the image ran as uid 65532, which meant every bind-mounted folder had to be
# chowned to that uid first; a step a user had to know about before
# `docker compose up -d` would work. Root in a container with no shell and no
# package manager, a read-only root filesystem and no-new-privileges (both in the
# compose file) is the trade chosen for a setup with no preparation.
FROM gcr.io/distroless/static

COPY --from=builder /out/opendrived /usr/local/bin/opendrived
COPY --from=builder /out/odctl /usr/local/bin/odctl

# /data holds everything the bridge keeps: credentials.key (written when you sign
# in), transfer state in jobs/, and the cache if it is switched on. Mount a folder
# or a volume over it, or every restart is a fresh install.
COPY --from=builder /out/data /data
WORKDIR /data

# Loopback would be unreachable from outside the container, so the image binds all
# interfaces. No API key is required (1.3); the compose file publishes the port to
# 127.0.0.1 only, and the daemon logs a warning when it listens beyond loopback
# without one. Set ODB_API_KEY to require one.
ENV ODB_LISTEN=0.0.0.0:9750

EXPOSE 9750

# There is no shell in this image, so the healthcheck is the binary itself.
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
    CMD ["/usr/local/bin/odctl", "--addr", "127.0.0.1:9750", "status"]

# No --cache-dir here: the gateway holds bytes OpenDrive does not have yet in
# write-back mode, so it is switched on by a deployment that has mounted /data
# somewhere durable — which the compose file does (ODB_CACHE_DIR=/data/cache).
ENTRYPOINT ["/usr/local/bin/opendrived", \
            "--keystore-file", "/data/credentials.key", \
            "--state-dir", "/data/jobs"]
