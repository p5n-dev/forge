VERSION ?= dev
LDFLAGS := -ldflags "-X sbp.gitlab.schubergphilis.com/Security/tools/forge/cmd.version=$(VERSION)"

.PHONY: build test lint clean

build:
	go build $(LDFLAGS) -o forge .

test:
	go test ./...

lint:
	golangci-lint run

clean:
	rm -f forge
