TEST_DATABASE_URL ?= postgres://postgres:dev@localhost:5432/spawner?sslmode=disable
TEST_NATS_URL ?= nats://localhost:4222
IMAGE ?= ghcr.io/biodigitaljaz/agent-spawner:0.1.64

.PHONY: build test test-integration deps run image

build:
	go build -o bin/spawner ./cmd/spawner

test:
	go test -race ./...

deps:
	docker compose up -d

test-integration: deps
	TEST_DATABASE_URL="$(TEST_DATABASE_URL)" TEST_NATS_URL="$(TEST_NATS_URL)" go test -race -count=1 ./...

run: deps build
	DATABASE_URL="$(TEST_DATABASE_URL)" NATS_URL="$(TEST_NATS_URL)" ./bin/spawner migrate
	DATABASE_URL="$(TEST_DATABASE_URL)" NATS_URL="$(TEST_NATS_URL)" ./bin/spawner all

image:
	docker build --build-arg VERSION=$(lastword $(subst :, ,$(IMAGE))) -t $(IMAGE) .
