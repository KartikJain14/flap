# Flap build and verification targets. Requires Go 1.26+ (go.mod pins the
# go1.27.3 toolchain, which the go command downloads automatically).

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo v1.0.0)
LDFLAGS := -s -w -X main.version=$(VERSION)
BUILD   := CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)"

STATICCHECK := honnef.co/go/tools/cmd/staticcheck@v0.8.2
GOVULNCHECK := golang.org/x/vuln/cmd/govulncheck@v1.9.0
SHELLCHECK  := koalaman/shellcheck:v0.10.0

.PHONY: all build build-arm64 test test-short race vet lint vuln shellcheck check \
	shots deploy-test play clean

all: build

## build: static Linux binaries in ./bin (flapd and the flapload load tool)
build:
	$(BUILD) -o bin/flapd ./cmd/flapd
	CGO_ENABLED=0 go build -trimpath -o bin/flapload ./cmd/flapload

## build-arm64: flapd for 64-bit ARM servers
build-arm64:
	GOOS=linux GOARCH=arm64 $(BUILD) -o bin/flapd-linux-arm64 ./cmd/flapd

## test: all unit and integration tests
test:
	go test ./...

## test-short: skip the slow tests
test-short:
	go test -short ./...

## race: tests with the race detector (needs cgo and a C toolchain)
race:
	CGO_ENABLED=1 go test -race ./...

vet:
	go vet ./...

## lint: staticcheck (pinned version)
lint:
	go run $(STATICCHECK) ./...

## vuln: govulncheck (pinned version)
vuln:
	go run $(GOVULNCHECK) ./...

## shellcheck: lint deployment scripts (needs Docker)
shellcheck:
	docker run --rm -v "$(CURDIR):/mnt:ro" -w /mnt $(SHELLCHECK) deploy/*.sh deploy/test/*.sh

## check: everything that does not need Docker
check: vet lint test race vuln

## shots: regenerate docs/screenshots (needs ImageMagick for PNGs)
shots:
	rm -rf docs/screenshots/svg && go test ./internal/tui -run TestShots -count=1 -shots=$(CURDIR)/docs/screenshots/svg
	./docs/screenshots/render.sh

## deploy-test: install/upgrade/rollback/uninstall in systemd containers (needs Docker)
deploy-test: build
	deploy/test/systemd-test.sh debian:12
	deploy/test/systemd-test.sh ubuntu:24.04

## play: play locally in this terminal
play: build
	./bin/flapd play

clean:
	rm -rf bin
