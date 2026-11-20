GO = go

PROG = blob

all: $(PROG)

$(PROG):
	$(GO) build -o $@
clean:
	$(RM) $(PROG)
build:
	$(GO) build ./...
test:
	$(GO) test ./...
race:
	$(GO) test ./... -race
vet:
	$(GO) vet ./...

.PHONY: all clean build test race vet

lint: go-lint md-lint sh-lint

go-lint:
	golangci-lint run
md-lint:
	markdownlint-cli2 *.md
sh-lint:
	shellcheck e2e/*.sh e2e/*.bash

.PHONY: lint go-lint md-lint sh-lint

gate: build test lint end2end

end2end: $(PROG)
	cd e2e && ./run.sh

.PHONY: end2end gate
