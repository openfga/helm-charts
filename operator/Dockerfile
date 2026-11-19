# pinned multi-arch index for golang:1.26.8 (linux/amd64, linux/arm64, ...)
FROM --platform=$BUILDPLATFORM golang:1.27.3@sha256:e519d3b0a5b285e5aa53ab9eef02bc6bbb9cb04608389b743a136a9ce75f0d1e AS builder

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
FROM gcr.io/distroless/static:nonroot@sha256:aafe993665bf6200db38a02c11a9351e782a84b6a45e7f33b6695638c88e3931
WORKDIR /
COPY --from=builder /operator .
USER 65532:65532

ENTRYPOINT ["/operator"]
