.PHONY: test-unit test-integration test-all check example-code-review build-echo-plugin

test-unit:
	go test -race ./...

test-integration:
	docker compose -f docker-compose.test.yml up -d --wait
	@# The go test exit status must win. The previous shape ended with
	@# `; docker compose down`, which overwrote the go test status and
	@# silently masked failing integration runs (audit: false-green
	@# integration gate). A trap runs teardown on exit while preserving
	@# whatever status go test returned.
	@trap 'docker compose -f docker-compose.test.yml down' EXIT; \
	REDIS_URL=redis://localhost:6379 \
	DATABASE_URL=postgres://postgres:postgres@localhost:5432/overlord_test?sslmode=disable \
	go test -race -tags integration ./...

test-all: test-unit test-integration

check: test-all
	go vet ./...
	staticcheck ./...

build-echo-plugin:
	go build -buildmode=plugin -o examples/plugins/echo/echo.so ./examples/plugins/echo/

example-code-review:
	go run ./cmd/overlord submit \
		--config config/examples/code_review.yaml \
		--id code-review \
		--payload @examples/code_review/sample_input.json \
		--wait \
		--timeout 3m
