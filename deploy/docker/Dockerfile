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

FROM --platform=$BUILDPLATFORM golang:1.24-alpine AS builder

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
