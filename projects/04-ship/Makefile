VERSION ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo dev)
IMAGE   ?= agent-ship

.PHONY: test run build docker-build docker-run

test:
	go vet ./...
	go test -race -count=1 ./...

run:
	TOKEN_INTERVAL_MS=150 go run -ldflags "-X main.version=$(VERSION)" .

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o bin/server .

docker-build:
	docker build --build-arg VERSION=$(VERSION) -t $(IMAGE):$(VERSION) .

# --env-file 传密钥；--init 不需要，因为 server 自己处理 SIGTERM
docker-run:
	docker run --rm -p 8080:8080 --env-file .env $(IMAGE):$(VERSION)
