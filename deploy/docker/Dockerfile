# The container image (whitepaper §8.3).
#
# Two things shape it. First, there is nothing in the runtime layer but the two
# programs and a set of CA certificates — no shell, no package manager, nothing
# for an intruder to pick up. Second, a container has no system keychain, so the
# bridge uses its encrypted-file credential store here and needs a key; if one is
# missing it refuses to start and says which environment variable to set. That
# refusal is the "no persistence = configuration error" rule of §9.2 doing its
# job, not a rough edge.
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
      -o /out/odctl ./cmd/odctl

# distroless/static:nonroot carries CA certificates, /etc/passwd and nothing else.
FROM gcr.io/distroless/static:nonroot

COPY --from=builder /out/opendrived /usr/local/bin/opendrived
COPY --from=builder /out/odctl /usr/local/bin/odctl

# /data holds the encrypted credentials and the transfer state. Mount a volume
# over it, or every restart is a fresh install.
WORKDIR /data
USER nonroot:nonroot

# Loopback is the safe default everywhere else, but inside a container it means
# "unreachable", so the image binds all interfaces — which is exactly why the
# daemon then insists on an API key. Publish the port to 127.0.0.1 on the host.
ENV ODB_LISTEN=0.0.0.0:9750

EXPOSE 9750

# There is no shell in this image, so the healthcheck is the binary itself.
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
    CMD ["/usr/local/bin/odctl", "--addr", "127.0.0.1:9750", "status"]

ENTRYPOINT ["/usr/local/bin/opendrived", \
            "--keystore", "encrypted_file", \
            "--keystore-file", "/data/credentials.enc", \
            "--state-dir", "/data/jobs"]
