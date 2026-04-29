SHELL := /usr/bin/env bash

IMAGE      ?= ghcr.io/hbinhng/coredns-crd
TAG        ?= dev
COREDNS_VER?= v1.12.0
GO_VER     ?= 1.23

.PHONY: tidy build test image install-crd uninstall-crd install-rbac uninstall-rbac apply-example

tidy:
	go mod tidy

build:
	go build ./...

test:
	go test ./...

image:
	docker build \
		--build-arg COREDNS_VERSION=$(COREDNS_VER) \
		--build-arg GO_VERSION=$(GO_VER) \
		-t $(IMAGE):$(TAG) .

install-crd:
	kubectl apply -f config/crd/

uninstall-crd:
	kubectl delete -f config/crd/

install-rbac:
	kubectl apply -f config/rbac/

uninstall-rbac:
	kubectl delete -f config/rbac/

apply-example:
	kubectl apply -f config/example/dnsslice.yaml
