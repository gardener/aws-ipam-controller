# SPDX-FileCopyrightText: Contributors to the Gardener project
#
# SPDX-License-Identifier: Apache-2.0

ENSURE_GARDENER_MOD         := $(shell go get github.com/gardener/gardener@$$(go list -m -f "{{.Version}}" github.com/gardener/gardener))
ENSURE_GARDENER_TOOLS_MOD   := $(shell go get github.com/gardener/gardener/hack/tools@$$(go list -m -f "{{.Version}}" github.com/gardener/gardener/hack/tools))
GARDENER_HACK_DIR           := $(shell go list -m -f "{{.Dir}}" github.com/gardener/gardener)/hack
REGISTRY              := europe-docker.pkg.dev/gardener-project/public
EXECUTABLE            := aws-ipam-controller
PROJECT               := github.com/gardener/aws-ipam-controller
IMAGE_REPOSITORY      := $(REGISTRY)/gardener/aws-ipam-controller
REPO_ROOT             := $(shell dirname $(realpath $(lastword $(MAKEFILE_LIST))))
HACK_DIR              := $(REPO_ROOT)/hack
VERSION               := $(shell cat VERSION)
IMAGE_TAG             := $(VERSION)
EFFECTIVE_VERSION     := $(VERSION)-$(shell git rev-parse HEAD)
GOARCH                := amd64

TOOLS_DIR := $(HACK_DIR)/tools
include $(GARDENER_HACK_DIR)/tools.mk

.PHONY: tidy
tidy:
	go mod tidy

.PHONY: clean
clean:
	@bash $(GARDENER_HACK_DIR)/clean.sh . ./pkg/...

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
check: $(GOIMPORTS) $(GOLANGCI_LINT) $(HELM)
	@bash $(GARDENER_HACK_DIR)/check.sh . ./pkg/...

# Run go fmt against code
.PHONY: format
format: $(GOIMPORTS) $(GOIMPORTSREVISER)
	@bash $(GARDENER_HACK_DIR)/format.sh ./main.go ./pkg

.PHONY: docker-images
docker-images:
	@docker build -t $(IMAGE_REPOSITORY):$(IMAGE_TAG) -f Dockerfile .

.PHONY: docker-images-linux-amd64
docker-images-linux-amd64:
	@docker buildx build --platform linux/amd64 -t $(IMAGE_REPOSITORY):$(IMAGE_TAG) -f Dockerfile .

.PHONY: check-generate
check-generate:
	@bash $(GARDENER_HACK_DIR)/check-generate.sh $(REPO_ROOT)

.PHONY: generate
generate: $(CONTROLLER_GEN) $(CRD_REF_DOCS) $(HELM) $(MOCKGEN) $(KUSTOMIZE) $(YQ) $(EXTENSION_GEN) $(KUBECTL)
	@REPO_ROOT=$(REPO_ROOT) CONTROLLER_GEN=$(CONTROLLER_GEN) GARDENER_HACK_DIR=$(GARDENER_HACK_DIR) bash $(GARDENER_HACK_DIR)/generate-sequential.sh . ./pkg/...
	$(MAKE) format
	@GARDENER_HACK_DIR=$(GARDENER_HACK_DIR) RENOVATE_CONFIG=$(REPO_ROOT)/.github/renovate.json5 bash $(GARDENER_HACK_DIR)/generate-renovate-ignore-deps.sh

.PHONY: sast
sast: $(GOSEC)
	@./hack/sast.sh

.PHONY: sast-report
sast-report: $(GOSEC)
	@./hack/sast.sh --gosec-report true

# Run tests
.PHONY: test
test:
	@env go test ./pkg/...

.PHONY: update-dependencies
update-dependencies:
	@env go get -u
	@make tidy

.PHONY: verify
verify: check format test sast-report