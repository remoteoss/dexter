.PHONY: build test release

build:
	go build -o dexter ./cmd/

test:
	go test ./...

lint:
	golangci-lint run ./...
	go mod tidy && git diff --exit-code go.mod go.sum

release:
	@if [ -z "$(VERSION)" ]; then echo "Usage: make release VERSION=0.2.0"; exit 1; fi
	@git checkout -b release-v$(VERSION)
	@sed -i '' 's/"0\.[0-9]*\.[0-9]*"/"$(VERSION)"/' internal/version/version.go
	@git add internal/version/version.go
	@git commit -m "Release v$(VERSION)"
	@echo "Release branch 'release-v$(VERSION)' created. Push it, merge into main, then run: make tag VERSION=$(VERSION)"

tag:
	@if [ -z "$(VERSION)" ]; then echo "Usage: make tag VERSION=0.2.0"; exit 1; fi
	@git checkout main
	@git pull origin main
	@git tag v$(VERSION)
	@git push origin v$(VERSION)
	@echo "Tagged and pushed v$(VERSION)"
