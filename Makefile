all: backend indexer

test: test_backend test_indexer test_shared

backend:
	cd backend && \
	go build -v -o search-engine

indexer:
	cd indexer && \
	go build -v -o indexer

test_backend:
	cd backend && \
	go test -v ./...

test_indexer:
	cd indexer && \
	go test -v ./...

test_shared:
	cd shared && \
	go test -v ./...


.PHONY: backend indexer test_backend test_indexer test_shared test
