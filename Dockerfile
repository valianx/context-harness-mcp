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
# khctl does not need CGO (no ONNX bindings) — build as a portable static binary.
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /out/khctl ./cmd/khctl

# Stage 2: build the goose migration binary.
# Runs as a separate stage so the goose download is cached independently of
# application code changes. Built with -ldflags="-s -w" to strip debug symbols
# and no_postgres=false / no_mysql=true etc. to include only the postgres driver,
# keeping the binary small (goose bundles many DB drivers by default).
FROM golang:1.23 AS goose-builder

RUN go install -ldflags="-s -w" -tags "no_mysql no_sqlite3 no_mssql no_redshift no_tidb no_clickhouse no_vertica no_ydb no_turso" \
    github.com/pressly/goose/v3/cmd/goose@v3.26.0

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
    && rm -rf /tmp/onnxruntime* \
    # onnxruntime_go (used by fastembed-go) dlopen("onnxruntime.so") by default.
    # The release archive provides libonnxruntime.so; create the bare symlink so
    # dlopen resolves without requiring SetSharedLibraryPath in application code.
    && ln -s /usr/local/lib/libonnxruntime.so /usr/local/lib/onnxruntime.so

# Copy binaries from builder stages.
COPY --from=builder /out/server /usr/local/bin/server
COPY --from=builder /out/khctl /usr/local/bin/khctl
COPY --from=goose-builder /go/bin/goose /usr/local/bin/goose

# Bake the migrations directory so the `migrate` compose profile can run
# `goose -dir /migrations` without a host bind-mount. This is the same
# /migrations path that deploy.yml (PR-7) will reference on Render.
COPY --from=builder /src/migrations /migrations

# Pre-download the fastembed-go model (all-MiniLM-L6-v2) into the image.
# fastembed-go defaults cache dir to "local_cache" (relative to CWD, which is
# "/" in this image). The GCS path fast-all-MiniLM-L6-v2.tar.gz returned 403
# as of 2026-05; the identically-named sentence-transformers archive at the
# same bucket remains public and extracts to the same fast-all-MiniLM-L6-v2/
# directory that fastembed-go checks at startup. Baking the model eliminates
# the runtime download and makes cold starts deterministic.
RUN curl -fsSL \
        "https://storage.googleapis.com/qdrant-fastembed/sentence-transformers-all-MiniLM-L6-v2.tar.gz" \
        -o /tmp/model.tar.gz \
    && mkdir -p /local_cache \
    && tar -xzf /tmp/model.tar.gz --exclude='._*' -C /local_cache \
    && rm /tmp/model.tar.gz \
    # fastembed-go v1.0.0 hardcodes the filename "model_optimized.onnx" (line 186
    # in fastembed.go), but the publicly-available archive ships "model.onnx".
    # The files are functionally identical; the symlink bridges the name mismatch.
    && ln -s /local_cache/fast-all-MiniLM-L6-v2/model.onnx \
             /local_cache/fast-all-MiniLM-L6-v2/model_optimized.onnx

# Default to stdio transport; docker-compose and Render override via CMD or
# environment variables passed at run time.
EXPOSE 7654
ENTRYPOINT ["/usr/local/bin/server"]
CMD ["-transport=stdio"]
