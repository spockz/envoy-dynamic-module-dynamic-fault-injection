# Build the Go dynamic module
FROM --platform=$BUILDPLATFORM golang:1.23 AS builder

ARG ZIG_VERSION=0.14.0
RUN apt update && apt install -y curl xz-utils
RUN curl -L "https://ziglang.org/download/${ZIG_VERSION}/zig-linux-$(uname -m)-${ZIG_VERSION}.tar.xz" | tar -J -x -C /usr/local && \
    ln -s "/usr/local/zig-linux-$(uname -m)-${ZIG_VERSION}/zig" /usr/local/bin/zig

WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CC="zig cc -target aarch64-linux-gnu" CXX="zig c++ -target aarch64-linux-gnu" CGO_ENABLED=1 GOARCH=arm64 go build -buildmode=c-shared -o /build/arm64_liblatency_fault_module.so .
RUN CC="zig cc -target x86_64-linux-gnu" CXX="zig c++ -target x86_64-linux-gnu" CGO_ENABLED=1 GOARCH=amd64 go build -buildmode=c-shared -o /build/amd64_liblatency_fault_module.so .

# Final image with Envoy
FROM envoyproxy/envoy:v1.37.0
ARG TARGETARCH
ENV ENVOY_DYNAMIC_MODULES_SEARCH_PATH=/usr/local/lib
COPY --from=builder /build/${TARGETARCH}_liblatency_fault_module.so /usr/local/lib/liblatency_fault_module.so
