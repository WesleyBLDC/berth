export DEVELOPER_DIR ?= /Applications/Xcode.app/Contents/Developer

.PHONY: hooks lint fmt check

hooks: ## Install the git hooks and prepare their environments
	prek install
	prek prepare-hooks

fmt: ## Format Go, Swift, Protobuf and Python
	for mod in $$(find . -name go.mod -not -path '*/.build/*' -not -path './spike/*' -exec dirname {} \;); do (cd $$mod && golangci-lint fmt --config $(CURDIR)/.golangci.yml ./...); done
	for mod in $$(find . -name buf.yaml -not -path '*/.build/*' -not -path './spike/*' -exec dirname {} \;); do (cd $$mod && buf format -w); done
	find . -name '*.swift' -not -path '*/.build/*' -not -path './spike/*' -not -path '*/SpikeProto/*' -print0 | xargs -0 xcrun swift format -i
	uvx ruff format --exclude spike .

lint: ## Every linter and hook over every file
	prek run --all-files

check: lint ## What must pass before a commit; tests join once M1 adds them
