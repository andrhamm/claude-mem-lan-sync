BINARY := cmemlan
PKG := github.com/andrhamm/claude-mem-lan-sync
VERSION ?= dev
LDFLAGS := -s -w -X $(PKG)/internal/buildinfo.Version=$(VERSION)

.PHONY: build test race vet lint fuzz e2e clean

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BINARY) ./cmd/cmemlan

test:
	go test ./...

# -race needs cgo, which release builds must not use. Kept in its own target so
# the two settings cannot leak into each other.
race:
	CGO_ENABLED=1 go test -race ./...

vet:
	go vet ./...

lint:
	golangci-lint run

# Seed corpora run with the normal test target; this is the real fuzzing pass.
fuzz:
	go test ./internal/proto -run xxx -fuzz FuzzOpRoundTrip -fuzztime 60s

# Requires node and a claude-mem install. Uses a scratch data dir and a
# non-default worker port so it cannot touch a developer's live memory.
e2e:
	go test -tags e2e ./test/e2e/...

clean:
	rm -f $(BINARY)
	go clean -testcache
