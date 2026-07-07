#include variables from .env file
include .env
export

# Database Commands
db-status:
	goose -dir migrations postgres "$(DATABASE_URL)" status

db-up:
	goose -dir migrations postgres "$(DATABASE_URL)" up

db-down:
	goose -dir migrations postgres "$(DATABASE_URL)" down

#Server commands
run:
	go run  .


