BINARY := claude-agents-mcp

.PHONY: all build lint test tidy run clean

all: tidy lint build

build:
	go build -o $(BINARY) ./cmd/claude-agents-mcp

# Mandatory lint step. CI and `make all` must pass this.
lint:
	golangci-lint run ./...

test:
	go test ./...

tidy:
	go mod tidy

run: build
	./$(BINARY)

clean:
	rm -f $(BINARY)
