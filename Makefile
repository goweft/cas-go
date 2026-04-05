BINARY := cas
MODULE := github.com/goweft/cas
BUILD_FLAGS := -ldflags="-s -w"

.PHONY: build test run clean install

build:
	go build $(BUILD_FLAGS) -o $(BINARY) ./cmd/cas

test:
	go test ./...

run: build
	CAS_PROVIDER=anthropic ./$(BINARY)

install: build
	cp $(BINARY) /usr/local/bin/$(BINARY)

clean:
	rm -f $(BINARY)

lint:
	go vet ./...
