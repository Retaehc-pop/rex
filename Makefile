PREFIX  ?= $(HOME)/.local
BINDIR  ?= $(PREFIX)/bin

.PHONY: build install uninstall

build:
	go build -o rex  ./cmd/rex
	go build -o rexd ./cmd/rexd

install:
	mkdir -p $(BINDIR)
	go build -o $(BINDIR)/rex  ./cmd/rex
	go build -o $(BINDIR)/rexd ./cmd/rexd
	@echo "Installed rex and rexd to $(BINDIR)"

uninstall:
	rm -f $(BINDIR)/rex $(BINDIR)/rexd
