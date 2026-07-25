.PHONY: image up down test backend-test frontend-test

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
