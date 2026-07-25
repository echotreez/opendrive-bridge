# Multi-stage build — whitepaper §8.3
FROM golang:1.22 AS builder
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION:-dev}" -o /out/opendrived ./cmd/opendrived \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION:-dev}" -o /out/odctl ./cmd/odctl

FROM gcr.io/distroless/static:nonroot
COPY --from=builder /out/opendrived /opendrived
COPY --from=builder /out/odctl /odctl
USER nonroot
EXPOSE 9750
ENTRYPOINT ["/opendrived"]
