.PHONY: test fuzz lint

test:
	go test -race ./...
	hack/check-pr-title-test.sh
	lab/gha/reduce-record-test.sh

fuzz:
	go test -run '^$$' -fuzz '^FuzzReadArchive$$' -fuzztime 50000x ./internal/collect
	go test -run '^$$' -fuzz '^FuzzReadStatus$$' -fuzztime 50000x ./internal/collect
	go test -run '^$$' -fuzz '^FuzzDecodeRecord$$' -fuzztime 50000x ./internal/collect
	go test -run '^$$' -fuzz '^FuzzRedactText$$' -fuzztime 50000x ./internal/collect
	go test -run '^$$' -fuzz '^FuzzRedactOp$$' -fuzztime 50000x ./internal/collect
	go test -run '^$$' -fuzz '^FuzzKnownSet$$' -fuzztime 50000x ./internal/collect
	go test -run '^$$' -fuzz '^FuzzScrub$$' -fuzztime 50000x ./internal/collect

lint:
	golangci-lint run ./...
