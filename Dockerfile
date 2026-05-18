# Stage 1: build the server binary.
# CGO_ENABLED=1 is required for fastembed-go's ONNX bindings (PR-5).
# The skeleton in PR-1 compiles cleanly without fastembed; this flag is set
# now so the build layer does not change when PR-5 adds the embedding code.
FROM golang:1.23 AS builder

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=1 go build -ldflags="-s -w" -o /out/server ./cmd/server

# Stage 2: build the goose migration binary.
# Runs as a separate stage so the goose download is cached independently of
# application code changes.
FROM golang:1.23 AS goose-builder

RUN go install github.com/pressly/goose/v3/cmd/goose@v3.26.0

# Stage 3: runtime image.
# debian:bookworm-slim ships glibc which is required for the ONNX shared
# library. Alpine/musl is not compatible with the official ONNX Runtime
# Linux x64 release.
FROM debian:bookworm-slim AS runtime

# Install ca-certificates and curl (healthcheck), then the ONNX runtime.
# The ONNX runtime version is pinned to match fastembed-go's tested release.
# PR-5 will verify exact version compatibility when adding fastembed-go.
ENV ONNX_RUNTIME_VERSION=1.20.0
ENV LD_LIBRARY_PATH=/usr/local/lib

RUN apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates \
        curl \
    && rm -rf /var/lib/apt/lists/* \
    && curl -fsSL \
        "https://github.com/microsoft/onnxruntime/releases/download/v${ONNX_RUNTIME_VERSION}/onnxruntime-linux-x64-${ONNX_RUNTIME_VERSION}.tgz" \
        -o /tmp/onnxruntime.tgz \
    && tar -xzf /tmp/onnxruntime.tgz -C /tmp \
    && cp /tmp/onnxruntime-linux-x64-${ONNX_RUNTIME_VERSION}/lib/libonnxruntime.so* /usr/local/lib/ \
    && ldconfig \
    && rm -rf /tmp/onnxruntime*

# Copy binaries from builder stages.
COPY --from=builder /out/server /usr/local/bin/server
COPY --from=goose-builder /go/bin/goose /usr/local/bin/goose

# Default to stdio transport; docker-compose and Render override via CMD or
# environment variables passed at run time.
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/server"]
CMD ["-transport=stdio"]
