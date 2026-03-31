.PHONY: build test release

build:
	go build -o dexter ./cmd/

test:
	go test ./...

release:
	@if [ -z "$(VERSION)" ]; then echo "Usage: make release VERSION=0.2.0"; exit 1; fi
	@sed -i '' 's/"0\.[0-9]*\.[0-9]*"/"$(VERSION)"/' internal/version/version.go
	@git add internal/version/version.go
	@git commit -m "Release v$(VERSION)"
	@git tag v$(VERSION)
	@git push origin main v$(VERSION)
	@echo "Released v$(VERSION)"
