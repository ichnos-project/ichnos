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
	go test -run '^$$' -fuzz '^FuzzCanonicalHash$$' -fuzztime 50000x ./internal/collect
	go test -run '^$$' -fuzz '^FuzzReadOp$$' -fuzztime 50000x ./internal/collect
	go test -run '^$$' -fuzz '^FuzzStringList$$' -fuzztime 50000x ./internal/collect
	go test -run '^$$' -fuzz '^FuzzSnapshot$$' -fuzztime 50000x ./internal/collect
	go test -run '^$$' -fuzz '^FuzzFingerprintText$$' -fuzztime 50000x ./pkg/snapshot

lint:
	golangci-lint run ./...
