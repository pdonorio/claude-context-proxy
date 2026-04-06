BINARY  := claude-context-proxy
INSTALL := $(HOME)/.local/bin/$(BINARY)

.PHONY: build run install test clean

build:
	go build -o $(BINARY) .

run: build
	./$(BINARY)

install: build
	mkdir -p $(HOME)/.local/bin
	cp $(BINARY) $(INSTALL)
	@echo "Installed to $(INSTALL)"

test:
	go test ./... -v

clean:
	rm -f $(BINARY)
