export GOWORK := off

.PHONY: test vet vectors vectors-check verify

test:
	go test -race -count=2 ./...

vet:
	go vet ./...

vectors:
	go run ./cmd/vectors

vectors-check:
	go run ./cmd/vectors -check

verify: vet test vectors-check
