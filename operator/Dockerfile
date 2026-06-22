# pinned multi-arch index for golang:1.26.2 (linux/amd64, linux/arm64, ...)
FROM --platform=$BUILDPLATFORM golang:1.26.2@sha256:5f3787b7f902c07c7ec4f3aa91a301a3eda8133aa32661a3b3a3a86ab3a68a36 AS builder

# buildx provides these automatically; declare so Go cross-compiles to the
# requested target instead of the build host's arch.
ARG TARGETOS
ARG TARGETARCH

WORKDIR /workspace
COPY go.mod go.sum ./
RUN go mod download

COPY cmd/ cmd/
COPY internal/ internal/

RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -ldflags="-s -w" -o /operator ./cmd/

# pinned multi-arch index for gcr.io/distroless/static:nonroot
FROM gcr.io/distroless/static:nonroot@sha256:e3f945647ffb95b5839c07038d64f9811adf17308b9121d8a2b87b6a22a80a39
WORKDIR /
COPY --from=builder /operator .
USER 65532:65532

ENTRYPOINT ["/operator"]
