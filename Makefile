export DEVELOPER_DIR ?= /Applications/Xcode.app/Contents/Developer

.PHONY: build native gen test test-native e2e lint fmt vuln verify breaking check hooks

build: ## Build berthd and berth into bin/ and sign both, so each trusts the other with secrets
	go build -o bin/ ./cmd/...
	scripts/sign.sh bin/berth com.wesleybldc.berth.cli
	scripts/sign.sh bin/berthd com.wesleybldc.berth.daemon

native: ## Build and sign bin/berth-native with the Personal Team identity
	scripts/build-native.sh

gen: ## Regenerate code from proto/
	buf generate

test: ## Unit tests with the race detector
	go test -race ./internal/...

test-native: ## Swift unit tests for berth-native's core
	swift test --package-path native

e2e: ## End to end tests with the real binaries
	go test -count=1 ./test/e2e/...

lint: ## Every linter and hook over every file, untracked Go and Protobuf included
	scripts/lint-go.sh
	scripts/lint-proto.sh
	prek run --all-files

fmt: ## Format Go, Swift, Protobuf and Python
	golangci-lint fmt ./...
	buf format -w
	find . -name '*.swift' -not -path '*/.build/*' -print0 | xargs -0 -r xcrun swift format -i
	uvx ruff format .

vuln: ## Known vulnerabilities in dependencies
	go run golang.org/x/vuln/cmd/govulncheck@latest ./...

verify: ## Dependencies match go.sum
	go mod verify

breaking: ## Schema changes that would break existing clients
	@if git rev-parse --verify -q main >/dev/null && git cat-file -e main:buf.yaml 2>/dev/null; then buf breaking --against '.git#branch=main'; else echo "no schema on main yet"; fi

check: lint verify breaking test test-native e2e vuln ## Everything that must pass before a commit
	@echo "check: PASS"

hooks: ## Install the git hooks and prepare their environments
	prek install
	prek prepare-hooks
