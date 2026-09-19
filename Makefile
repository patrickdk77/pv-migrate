BRANCH      := $(shell git rev-parse --abbrev-ref HEAD)
SHA1        := $(shell git rev-parse HEAD)
SHORT_SHA1  := $(shell git rev-parse --short HEAD)
ORIGIN      := $(shell git remote get-url origin)
DATE        := $(shell date -u +'%Y-%m-%dT%H:%M:%SZ')
VER         := $(shell git describe --tags --abbrev=0 2>/dev/null || echo "v0.0.1")
DOCK_REPO   := docker.patrickdk.com/dswett/pv-migrate

export DOCKERFILE_PATH=Dockerfile
export DOCKER_REPO=$(DOCK_REPO)
export DOCKER_TAG=latest
export GIT_BRANCH=$(BRANCH)
export GIT_SHA1=$(SHA1)
export GIT_SHORT_SHA1=$(SHORT_SHA1)
export GIT_TAG=$(SHA1)
export GIT_VERSION=$(VER)
export GIT_VERSION_MAJOR=$(shell echo $(VER) | cut -f1 -d.)
export GIT_VERSION_MINOR=$(shell echo $(VER) | cut -f2 -d.)
export IMAGE_NAME=$(DOCKER_REPO):$(VER)
export SOURCE_BRANCH=$(BRANCH)
export SOURCE_COMMIT=$(SHA1)
export SOURCE_TYPE=git
export SOURCE_REPOSITORY_URL=$(ORIGIN)

PLATFORMS := linux/amd64,linux/arm64
MOVERS    := sshd rsync rclone
GOARCHES  := amd64 arm64
STAGE     := dist/cli

# goreleaser stamps main.version without the leading "v", and the CLI turns
# that back into "v<version>" to tag the data mover images it installs. Stamp
# the same string here, or a released CLI asks for a tag nothing pushed.
CLI_VERSION := $(VER:v%=%)
LDFLAGS     := -s -w -X main.version=$(CLI_VERSION) -X main.commit=$(SHA1) -X main.date=$(DATE)

all: buildx

buildx: movers cli

# The data mover images the chart pulls. They carry the CLI's own tag, so
# publishing one without the other leaves the CLI pulling a tag that is not
# there.
movers: $(addprefix mover-,$(MOVERS))

mover-%:
	docker buildx build --pull --push \
		--platform $(PLATFORMS) \
		--file docker/$*/Dockerfile \
		--tag $(DOCK_REPO)-$*:$(VER) \
		docker/$*/
	skopeo copy --all docker://$(DOCK_REPO)-$*:$(VER) docker://$(DOCK_REPO)-$*:$(GIT_VERSION_MAJOR).$(GIT_VERSION_MINOR)
	skopeo copy --all docker://$(DOCK_REPO)-$*:$(VER) docker://$(DOCK_REPO)-$*:$(GIT_VERSION_MAJOR)
	skopeo copy --all docker://$(DOCK_REPO)-$*:$(VER) docker://$(DOCK_REPO)-$*:latest

cli: stage
	docker buildx build --pull --push \
		--platform $(PLATFORMS) \
		--file $(DOCKERFILE_PATH) \
		--tag $(IMAGE_NAME) \
		$(STAGE)
	skopeo copy --all docker://$(IMAGE_NAME) docker://$(DOCKER_REPO):$(GIT_VERSION_MAJOR).$(GIT_VERSION_MINOR)
	skopeo copy --all docker://$(IMAGE_NAME) docker://$(DOCKER_REPO):$(GIT_VERSION_MAJOR)
	skopeo copy --all docker://$(IMAGE_NAME) docker://$(DOCKER_REPO):latest

# Dockerfile copies ${TARGETPLATFORM}/pv-migrate, so the context is a tree of
# per-platform binaries rather than the source.
stage:
	rm -rf $(STAGE)
	for arch in $(GOARCHES); do \
		mkdir -p $(STAGE)/linux/$$arch || exit 1; \
		CGO_ENABLED=0 GOOS=linux GOARCH=$$arch go build -trimpath \
			-ldflags "$(LDFLAGS)" \
			-o $(STAGE)/linux/$$arch/pv-migrate ./cmd/pv-migrate || exit 1; \
	done

# Build and load locally (single arch, no push)
local: $(addprefix local-,$(MOVERS))

local-%:
	docker buildx build --pull --load \
		--file docker/$*/Dockerfile \
		--tag $(DOCK_REPO)-$*:latest \
		docker/$*/

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o dist/pv-migrate ./cmd/pv-migrate

test:
	go vet ./...
	go test ./...

clean:
	rm -rf dist/

.PHONY: all buildx movers cli stage local build test clean
