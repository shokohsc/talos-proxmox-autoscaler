IMG ?= ghcr.io/your-org/talos-proxmox-autoscaler:latest

.PHONY: build test lint docker-build docker-push

build:
	CGO_ENABLED=0 go build -a -installsuffix cgo -o bin/talos-proxmox-autoscaler .

test:
	go test ./... -v

lint:
	golangci-lint run ./...

docker-build:
	docker build -t ${IMG} .

docker-push:
	docker push ${IMG}

generate:
	controller-gen object:headerFile="hack/boilerplate.go.txt paths="./..."

crd:
	controller-gen crd paths="./..." output:crd:dir=config/crd

fmt:
	go fmt ./...

vet:
	go vet ./...
