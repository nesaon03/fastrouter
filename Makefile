# Thin wrapper around scripts/build.sh (the script works without make).
.PHONY: build dist test npmtest pack clean

build:
	CGO_ENABLED=0 go build -trimpath -o fastrouter ./cmd/fastrouter

dist:
	./scripts/build.sh

test:
	go build ./... && go vet ./... && go test ./... && node --test npm/test.js

npmtest:
	node --test npm/test.js

pack:
	npm pack --dry-run

clean:
	rm -rf dist fastrouter fastrouter.exe
