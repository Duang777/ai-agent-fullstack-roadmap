.PHONY: test tokens

test:
	go vet ./...
	go test -race -count=1 -v ./...

tokens:
	uv run tokens.py
