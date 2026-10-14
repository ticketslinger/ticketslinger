.PHONY: dev-db dev-db-down test lint gen
export TEST_DATABASE_URL_OWNER ?= postgres://ts_owner:ts_owner@localhost:54329/ticketslinger?sslmode=disable
export TEST_DATABASE_URL_APP   ?= postgres://ts_app:ts_app@localhost:54329/ticketslinger?sslmode=disable

dev-db:      ; docker compose -f deploy/compose.dev.yaml up -d --wait
dev-db-down: ; docker compose -f deploy/compose.dev.yaml down -v
test:        ; cd api && go test -race ./... && cd .. && bun test --pass-with-no-tests
lint:        ; cd api && golangci-lint run ./... && cd .. && bunx biome ci --no-errors-on-unmatched .
gen:         ; cd api && go generate ./... && cd .. && bun run --filter '*' gen
