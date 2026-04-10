.PHONY: test-unit test-integration test-all check example-code-review build-echo-plugin

test-unit:
	go test -race ./...

test-integration:
	docker compose -f docker-compose.test.yml up -d --wait
	REDIS_URL=redis://localhost:6379 \
	DATABASE_URL=postgres://postgres:postgres@localhost:5432/orcastrator_test?sslmode=disable \
	go test -race -tags integration ./... ; \
	docker compose -f docker-compose.test.yml down

test-all: test-unit test-integration

check: test-all
	go vet ./...
	staticcheck ./...

build-echo-plugin:
	go build -buildmode=plugin -o examples/plugins/echo/echo.so ./examples/plugins/echo/

example-code-review:
	go run ./cmd/orcastrator submit \
		--config config/examples/code_review.yaml \
		--pipeline code-review \
		--payload @examples/code_review/sample_input.json \
		--wait \
		--timeout 3m
