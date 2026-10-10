GO ?= go
.PHONY: build app test clean
build:
	npm run build --prefix web
	$(GO) build -o Brodge ./cmd/brodge

app: build
	./scripts/package-macos.sh

test:
	$(GO) test ./...

clean:
	rm -f Brodge
