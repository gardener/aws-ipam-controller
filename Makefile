# SPDX-FileCopyrightText: 2024 SAP SE or an SAP affiliate company and Gardener contributors
#
# SPDX-License-Identifier: Apache-2.0

REGISTRY              := europe-docker.pkg.dev/gardener-project/public
EXECUTABLE            := aws-ipam-controller
PROJECT               := github.com/gardener/aws-ipam-controller
IMAGE_REPOSITORY      := $(REGISTRY)/gardener/aws-ipam-controller
REPO_ROOT             := $(shell dirname $(realpath $(lastword $(MAKEFILE_LIST))))
VERSION               := $(shell cat VERSION)
IMAGE_TAG             := $(VERSION)
EFFECTIVE_VERSION     := $(VERSION)-$(shell git rev-parse HEAD)
GOARCH                := amd64

TOOLS_DIR := hack/tools
include $(TOOLS_DIR)/tools.mk

.PHONY: tidy
tidy:
	go mod tidy

# build local executable
.PHONY: build-local
build-local:
	@CGO_ENABLED=1 go build -o $(EXECUTABLE) \
		-race \
		-ldflags "-X 'main.Version=$(EFFECTIVE_VERSION)' -X 'main.ImageTag=$(IMAGE_TAG)'"\
		main.go

.PHONY: release
release:
	@CGO_ENABLED=0 GOOS=linux GOARCH=$(GOARCH) go build -o $(EXECUTABLE) \
        -ldflags "-w -X 'main.Version=$(EFFECTIVE_VERSION)' -X 'main.ImageTag=$(IMAGE_TAG)'"\
		main.go

.PHONY: check
check: $(GOIMPORTS) $(GOLANGCI_LINT)
	go vet ./...
	GOIMPORTS=$(GOIMPORTS) GOLANGCI_LINT=$(GOLANGCI_LINT) hack/check.sh ./...

# Run go fmt against code
.PHONY: format
format: $(GOIMPORTS)
	$(GOIMPORTS) -l -w  main.go ./pkg

.PHONY: docker-images
docker-images:
	@docker build -t $(IMAGE_REPOSITORY):$(IMAGE_TAG) -f Dockerfile .

.PHONY: docker-images-linux-amd64
docker-images-linux-amd64:
	@docker buildx build --platform linux/amd64 -t $(IMAGE_REPOSITORY):$(IMAGE_TAG) -f Dockerfile .

.PHONY: generate
generate: $(MOCKGEN)
	@MOCKGEN=$(shell realpath $(MOCKGEN)) go generate ./pkg/...

# Run tests
.PHONY: test
test:
	@env go test ./pkg/...

.PHONY: update-dependencies
update-dependencies:
	@env go get -u
	@make tidy

.PHONY: verify
verify: check format test