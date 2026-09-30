.PHONY: all build docker clean

all: build

# Builds for Linux at the host's native architecture, since the binary is
# meant to be copied into a scratch container that runs on a Linux host.
# Cross-arch (e.g. building amd64 on an arm64 Mac) runs fine under QEMU for
# most things, but cross-build explicitly (ARCH=amd64 make build) only when
# targeting a different host.
ARCH ?= $(shell go env GOARCH)

build:
	CGO_ENABLED=0 GOOS=linux GOARCH=$(ARCH) go build -trimpath -ldflags="-s -w" -o mam-ratio .

docker: build
	docker build -t mam-ratio:local .

clean:
	rm -f mam-ratio
