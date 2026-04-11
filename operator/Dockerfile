# pinned golang:1.26.2 linux/amd64
FROM golang:1.26.2@sha256:b53c282df83967299380adbd6a2dc67e750a58217f39285d6240f6f80b19eaad AS builder

WORKDIR /workspace
COPY go.mod go.sum ./
RUN go mod download

COPY cmd/ cmd/
COPY internal/ internal/

RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /operator ./cmd/

# pinned gcr.io/distroless/static:nonroot linux/amd64
FROM gcr.io/distroless/static:nonroot@sha256:64c43684e6d2b581d1eb362ea47b6a4defee6a9cac5f7ebbda3daa67e8c9b8e6
WORKDIR /
COPY --from=builder /operator .
USER 65532:65532

ENTRYPOINT ["/operator"]
