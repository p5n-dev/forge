VERSION ?= dev
LDFLAGS := -ldflags "-X github.com/p5n-dev/forge/cmd.version=$(VERSION)"

.PHONY: build test lint clean

build:
	go build $(LDFLAGS) -o forge .

test:
	go test ./...

lint:
	golangci-lint run

clean:
	rm -f forge
