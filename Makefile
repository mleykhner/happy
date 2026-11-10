.PHONY: build ui test vendor
ui:
	cd web && npm ci --ignore-scripts && npm run build
build: ui
	test -d vendor || tar -xzf packaging/vendor.tar.gz
	mkdir -p bin
	CGO_ENABLED=0 go build -mod=vendor -trimpath -o bin/happy ./cmd/happy
test:
	test -d vendor || tar -xzf packaging/vendor.tar.gz
	go test -mod=vendor -race ./...
	go vet -mod=vendor ./...
vendor:
	go mod vendor
	python3 scripts/vendor.py
