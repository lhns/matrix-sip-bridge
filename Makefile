BINARY := matrix-sip-bridge
PKG    := github.com/lhns/matrix-sip-bridge
# goolm selects mautrix's pure-Go Olm implementation; the default CGO build
# wants libolm headers. CGO cannot be disabled: mxmain imports go-sqlite3.
TAGS    := goolm
GOFLAGS := -trimpath -tags $(TAGS)

TAG        := $(shell git describe --exact-match --tags 2>/dev/null)
COMMIT     := $(shell git rev-parse HEAD 2>/dev/null)
BUILD_TIME := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

LDFLAGS := -s -w \
	-X main.Tag=$(TAG) \
	-X main.Commit=$(COMMIT) \
	-X main.BuildTime=$(BUILD_TIME)

.PHONY: build test lint clean

build:
	go build $(GOFLAGS) -ldflags="$(LDFLAGS)" -o $(BINARY) .

test:
	go test -tags $(TAGS) ./... -count=1

lint:
	go vet -tags $(TAGS) ./...
	golangci-lint run --build-tags $(TAGS) ./...

clean:
	rm -f $(BINARY) $(BINARY).exe
