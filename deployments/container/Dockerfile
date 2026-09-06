# syntax=docker/dockerfile:1
# Copyright 2023 The Kubernetes Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

ARG GOLANG_VERSION=1.27
ARG BASE_IMAGE=quay.io/centos/centos:stream9

# Build stage using native architecture to avoid emulation issues during Go compilation
FROM --platform=$BUILDPLATFORM golang:${GOLANG_VERSION} AS build

# TARGETOS/TARGETARCH are set by BuildKit from the requested --platform
ARG TARGETOS
ARG TARGETARCH

WORKDIR /go/src/github.com/k8snetworkplumbingwg/dra-driver-sriov

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .

RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    mkdir /artifacts && \
    GOOS=${TARGETOS} GOARCH=${TARGETARCH} make PREFIX=/artifacts cmds

# Final stage using target architecture
FROM ${BASE_IMAGE}

ARG VERSION="N/A"
ARG RELEASE="N/A"

LABEL org.opencontainers.image.source=https://github.com/k8snetworkplumbingwg/dra-driver-sriov
LABEL org.opencontainers.image.description="Dynamic Resource Allocation (DRA) resource driver for SR-IOV devices"
LABEL org.opencontainers.image.licenses=Apache-2.0
LABEL org.opencontainers.image.title="Dynamic Resource Allocation (DRA) resource driver for SR-IOV devices"
LABEL org.opencontainers.image.vendor="sriovnetwork.k8snetworkplumbingwg.io"
LABEL org.opencontainers.image.version=${VERSION}
LABEL org.opencontainers.image.release=${RELEASE}
LABEL io.k8s.display-name="Dynamic Resource Allocation (DRA) resource driver for SR-IOV devices"
LABEL name="Dynamic Resource Allocation (DRA) resource driver for SR-IOV devices"
LABEL vendor="sriovnetwork.k8snetworkplumbingwg.io"
LABEL version=${VERSION}
LABEL release=${RELEASE}
LABEL summary="Dynamic Resource Allocation (DRA) resource driver for SR-IOV devices"
LABEL description="See summary"

RUN yum -y install hwdata pciutils delve && yum clean all
COPY --from=build /artifacts/dra-driver-sriov /usr/bin/dra-driver-sriov
