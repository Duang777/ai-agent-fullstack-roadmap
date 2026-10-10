export DATABASE_URL ?= postgres://postgres:postgres@localhost:5432/agent?sslmode=disable
export REDIS_URL    ?= redis://localhost:6379/0

.PHONY: up down test psql

up:
	docker compose up -d --wait

down:
	docker compose down -v

test:
	go vet ./...
	go test -race -count=1 -v ./...

psql:
	docker compose exec postgres psql -U postgres agent
