BIN ?= $(HOME)/.local/bin/tummy

.PHONY: build install test check fmt vet cover

build:
	go build ./...

install:
	go build -o $(BIN) ./cmd/tummy

test:
	go test ./...

fmt:
	@out="$$(gofmt -l cmd internal)"; if [ -n "$$out" ]; then echo "$$out"; exit 1; fi

vet:
	go vet ./...

check: fmt vet test

cover:
	go test -coverprofile=/tmp/tummy.cover ./... >/dev/null && go tool cover -func=/tmp/tummy.cover | tail -1
