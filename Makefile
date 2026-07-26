BINARY := idlerthing

.PHONY: build run test vet clean

build:
	go build -trimpath -ldflags "-s -w" -o $(BINARY) .

run:
	go run .

test:
	go test ./...

vet:
	@test -z "$$(gofmt -l .)" || (gofmt -l . && exit 1)
	go vet ./...

clean:
	rm -f $(BINARY)
