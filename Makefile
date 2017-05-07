GO_BIN := $(GOPATH)/bin
LIST_NO_VENDOR := $(go list ./... | grep -v /vendor/)

.PHONY: %

default: env fmt deps lame build

build:
	go build -a -o boa .

clean:
	go clean -i
	rm -rf files/
	rm -rf go/

deps:
	# Install or update govend
	go get -u github.com/govend/govend
	# Fetch vendored dependencies
	$(GO_BIN)/govend -v

env:
	mkdir -p files/stream && \
	mkdir -p files/converted-320 && \
	mkdir -p files/original-upload

fmt:
	go fmt $(LIST_NO_VENDOR)

generate-deps:
	# Generate vendor.yml
	govend -v -l
	git checkout vendor/.gitignore

lame:
	# Check for lame
	lame --help > /dev/null || echo "WARNING: LAME not found. Please install, boa will not work without it."
