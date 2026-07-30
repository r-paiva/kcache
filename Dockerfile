# SPDX-FileCopyrightText: Copyright (c) 2026, the latch developers
#
# SPDX-License-Identifier: Apache-2.0

FROM golang:1.26-bookworm AS builder

# BPF toolchain needed by go generate:
#   clang/llvm  — compile bpf/latch.c
#   libbpf-dev  — provides bpf/bpf_helpers.h and bpf/bpf_endian.h
#   linux-libc-dev — provides linux/bpf.h
RUN apt-get update && apt-get install -y --no-install-recommends \
        clang llvm libbpf-dev linux-libc-dev \
    && ln -sf /usr/include/*-linux-gnu/asm /usr/include/asm \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=dev
RUN go generate ./... \
    && CGO_ENABLED=0 GOOS=linux go build -trimpath \
       -ldflags "-X codeberg.org/latch/latch/internal/version.Version=${VERSION}" \
       -o latch .


FROM gcr.io/distroless/static-debian12

COPY --from=builder /src/latch /latch

ENTRYPOINT ["/latch"]
