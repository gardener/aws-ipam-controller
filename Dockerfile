# SPDX-FileCopyrightText: 2024 SAP SE or an SAP affiliate company and Gardener contributors
#
# SPDX-License-Identifier: Apache-2.0

############# builder
FROM golang:1.23.3 AS builder

WORKDIR /build

# Copy go mod and sum files
COPY go.mod go.sum ./
# Download all dependencies. Dependencies will be cached if the go.mod and go.sum files are not changed
RUN go mod download

COPY . .
ARG TARGETARCH
RUN make release GOARCH=$TARGETARCH

############# aws-ipam-controller
FROM gcr.io/distroless/static-debian11:nonroot AS aws-ipam-controller

COPY --from=builder /build/aws-ipam-controller /aws-ipam-controller
ENTRYPOINT ["/aws-ipam-controller"]