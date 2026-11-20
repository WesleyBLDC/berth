export DEVELOPER_DIR ?= /Applications/Xcode.app/Contents/Developer

.PHONY: build native app gen test test-native test-app e2e lint fmt vuln verify breaking check gate test-gate gitleaks-full hooks

build: ## Build berthd, berth, berth-proxy and berth-mcp into bin/; sign the three that trust each other with secrets
	go build -o bin/ ./cmd/...
	scripts/sign.sh bin/berth com.wesleybldc.berth.cli
	scripts/sign.sh bin/berthd com.wesleybldc.berth.daemon
	scripts/sign.sh bin/berth-proxy com.wesleybldc.berth.proxy

native: ## Build and sign bin/berth-native with the Personal Team identity
	scripts/build-native.sh

app: ## Build and sign bin/Berth.app with berthd, berth, berth-native and berth-proxy inside
	scripts/build-app.sh

gen: ## Regenerate code from proto/
	buf generate

test: ## Unit tests with the race detector
	go test -race ./internal/... ./cmd/...

test-native: ## Swift unit tests for berth-native's core
	swift test --package-path native
	@# The helper must stay alive through a modal session and an async wait, or Touch ID dies with no reply.
	@echo | perl -e 'alarm 20; exec @ARGV' native/.build/debug/berth-native selftest-wait | grep -q '"waited"' || { echo 'berth-native did not stay alive and reply through the main run loop'; exit 1; }

test-app: ## Swift unit tests for Berth.app's core, and the bundle's layout, signatures and plists signed ad hoc
	swift build --package-path macos
	swift test --package-path macos
	scripts/check-app-bundle.sh

e2e: ## End to end tests with the real binaries
	go test -count=1 ./test/e2e/...

lint: ## Every linter and hook over every file, untracked Go and Protobuf included
	scripts/lint-go.sh
	scripts/lint-proto.sh
	prek run --all-files

fmt: ## Format Go, Swift, Protobuf and shell
	golangci-lint fmt ./...
	buf format -w
	find . -name '*.swift' -not -path '*/.build/*' -print0 | xargs -0 -r xcrun swift format -i
	git ls-files -z '*.sh' | xargs -0 -r go run mvdan.cc/sh/v3/cmd/shfmt@v3.14.1 -w

vuln: ## Known vulnerabilities in dependencies
	go run golang.org/x/vuln/cmd/govulncheck@v1.9.0 ./...

verify: ## Dependencies match go.sum
	go mod verify

breaking: ## Schema changes that would break existing clients
	@if git rev-parse --verify -q main >/dev/null && git cat-file -e main:buf.yaml 2>/dev/null; then buf breaking --against '.git#branch=main'; else echo "no schema on main yet"; fi

test-gate: ## The gate's own test: a failing merge, a failing check and a dirty tree move nothing
	scripts/test-gate.sh

check: lint verify breaking test-gate test test-native test-app e2e vuln ## Everything that must pass before a commit
	@echo "check: PASS"

gitleaks-full: ## gitleaks over the whole history, at the version .pre-commit-config.yaml pins
	go run github.com/zricethezav/gitleaks/v8@$$(awk '/gitleaks\/gitleaks/{f=1;next} f&&/rev:/{print $$2;exit}' .pre-commit-config.yaml) git --no-banner --redact .

gate: ## make check through the gate, which fails unless it ends with "check: PASS"; merge and push with scripts/gate.sh
	scripts/gate.sh check

hooks: ## Install the git hooks and prepare their environments
	prek install
	prek prepare-hooks

.PHONY: fuzz fuzz-long
FUZZTIME ?= 10s
fuzz: ## Run every Fuzz test past its seed corpus for FUZZTIME each; make test runs the seeds alone, and crashers land in testdata/fuzz
	@grep -rl --include='*_test.go' '^func Fuzz' internal cmd | xargs -n1 dirname | sort -u | while read -r d; do \
		for f in $$(grep -ho '^func Fuzz[A-Za-z0-9_]*' "$$d"/*_test.go | cut -c6-); do \
			go test "./$$d" -run '^$$' -fuzz "^$$f$$" -fuzztime $(FUZZTIME) || exit 1; \
		done; \
	done

fuzz-long: ## The nightly lab's fuzzing: every Fuzz test for 5 minutes
	@$(MAKE) fuzz FUZZTIME=5m
