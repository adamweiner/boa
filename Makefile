SHELL := /bin/bash
PWD    = $(shell pwd)

.PHONY: %

default: env fmt deps build lame

build:
	GOPATH=$(PWD)/go/ go build -o boa boa.go

clean:
	go clean -i
	rm -rf files/
	rm -rf go/

deps:
	# Fetch Go deps using gpm
	curl -s https://raw.githubusercontent.com/pote/gpm/master/bin/gpm > gpm.sh
	chmod +x gpm.sh
	GOPATH=$(PWD)/go/ ./gpm.sh
	rm gpm.sh

env:
	mkdir -p files/stream && \
	mkdir -p files/converted-320 && \
	mkdir -p files/original-upload

fmt:
	go fmt ./...

lame:
	# Check for lame
	lame --help > /dev/null || echo "WARNING: LAME not found. Please install, boa will not work without it."
