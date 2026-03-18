swag:
	cd backend && swag init -g cmd/server/main.go --outputTypes json

dev-up:
	docker compose -f backend/docker-compose.yml up -d --build

dev-down:
	docker compose -f backend/docker-compose.yml down

dev-down-volumes:
	docker compose -f backend/docker-compose.yml down --volumes
