.PHONY: image up down test backend-test frontend-test lint-problems problems-init problems-update

## Build the k3s-base problem image (required before starting problems)
image:
	docker compose --profile image up --build k3s-base

## Start the full stack (never use down -v: pgdata holds dev data)
up:
	docker compose up --build -d

down:
	docker compose down

backend-test:
	cd backend && go test ./... -short -race

frontend-test:
	cd frontend && npx tsc --noEmit && npm run build

test: backend-test frontend-test

## Lint problem scripts (sh -n + shellcheck if installed)
lint-problems:
	@fail=0; \
	for f in problems/*/*.sh; do \
		sh -n "$$f" || fail=1; \
		if command -v shellcheck >/dev/null 2>&1; then shellcheck -S warning "$$f" || fail=1; fi; \
	done; \
	[ $$fail -eq 0 ] && echo "problem scripts OK"

## Fetch the public sample-problem submodule after a normal git clone.
problems-init:
	git submodule update --init --recursive

## Move the sample-problem submodule to the latest main commit.
problems-update:
	git submodule update --init --remote problems
