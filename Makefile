.PHONY: test all test-race test-fuzz lint clean bench

all: lint clean test test-race test-fuzz bench

test:
	go test -count=1 -v ./...

test-race:
	go test -race -count=1 -v ./...

test-fuzz:
	go test -fuzz=FuzzWaitingRoom -fuzztime=30s .

bench:
	go test -bench=. -benchmem ./...

lint:
	go vet ./...

clean:
	go clean ./...