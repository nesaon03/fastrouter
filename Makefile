# Thin wrapper around scripts/build.sh (the script works without make).
.PHONY: build dist test clean

build:
	CGO_ENABLED=0 go build -trimpath -o fastrouter ./cmd/fastrouter

dist:
	./scripts/build.sh

test:
	go build ./... && go vet ./... && go test ./...

clean:
	rm -rf dist fastrouter fastrouter.exe
