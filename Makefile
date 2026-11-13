GO ?= go
VERSION ?= development
TAGS ?=
OUTPUT ?= arednlink-hf
GOARCH ?= arm
GOARM ?= 7

.PHONY: build none kiss js8call both test

# Invoke only inside the designated remote build container.
build:
	CGO_ENABLED=0 GOOS=linux GOARCH=$(GOARCH) GOARM=$(GOARM) $(GO) build -trimpath -tags '$(TAGS)' -ldflags '-s -w -X main.version=$(VERSION)' -o '$(OUTPUT)' ./cmd/arednlink-hf

none:
	$(MAKE) build TAGS=

kiss:
	$(MAKE) build TAGS=kiss

js8call:
	$(MAKE) build TAGS=js8call

both:
	$(MAKE) build TAGS='kiss js8call'

test:
	$(GO) test -race -tags '$(TAGS)' ./...
