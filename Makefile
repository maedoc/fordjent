.PHONY: build run test lint clean

build:
	mkdir -p bin
	go build -o bin/fordjent ./cmd/fordjent
	go build -o bin/fj ./cmd/fj

run: build
	./bin/fordjent -config fordjent.local.yaml

test:
	go test ./... -count=1 -timeout 60s

lint:
	golangci-lint run ./...

clean:
	rm -rf bin/
