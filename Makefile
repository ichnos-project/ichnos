.PHONY: test fuzz lint

test:
	cd hack/tools && go test -race ./...
	hack/check-pr-title-test.sh
	lab/gha/reduce-record-test.sh

fuzz:
	cd hack/tools && go test -run '^$$' -fuzz '^FuzzCheck$$' -fuzztime 50000x ./internal/gitguard

lint:
	cd hack/tools && golangci-lint run ./...
