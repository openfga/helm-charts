IMG ?= openfga/openfga-operator:dev

.PHONY: build test vet fmt lint docker-build docker-push clean

build:
	mkdir -p bin
	go build -o bin/operator ./cmd/

test:
	go test ./... -v

vet:
	go vet ./...

fmt:
	go fmt ./...

lint: vet fmt

docker-build:
	docker build -t $(IMG) .

docker-push:
	docker push $(IMG)

clean:
	rm -rf bin/
