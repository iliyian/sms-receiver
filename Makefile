.PHONY: build run test vet fmt tidy docker

build:
	go build -trimpath -o bin/sms-receiver .

run:
	go run .

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -w main.go internal

tidy:
	go mod tidy

docker:
	docker build -t sms-receiver .
