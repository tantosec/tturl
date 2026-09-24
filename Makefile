# Developer tasks -- Linux/macOS dev tooling, not part of the shippable module.
# Scope defaults to the whole module; override for one package:
#   make check PKG=./tth2/...

PKG ?= ./...
FMT := $(PKG:/...=)                # ./... -> . ; ./tth2/... -> ./tth2
RANKING_PANEL ?= smoke
RANKING_RUNS ?= 20
RANKING_SEED_START ?= 1
RANKING_SOLVER ?= .*
RANKING_SCENARIO ?= .*
RANKING_OUTPUT ?=
RANKING_QUALIFICATION_SHARD ?= all
RANKING_CAMPAIGN_SEED_START ?=
RANKING_CAMPAIGN_RUN_ID ?=
RANKING_EVIDENCE_PATH ?=
RANKING_SUMMARY_OUTPUT ?=
RANKING_DISPOSITION_OUTPUT ?=
RANKING_REPLAY_SCENARIO ?=
RANKING_REPLAY_SOLVER ?=
RANKING_REPLAY_SEED ?=

.DEFAULT_GOAL := check
.DELETE_ON_ERROR:

# Pin remote Go tools that define repository output, diagnostics, or generated
# state. scripts/goreleaser.sh separately owns the release-artefact tool pin.
# The vulnerability scanner alone follows @latest: its executable and database
# are both deliberately current security inputs.
GOLANGCI_VERSION := v2.13.2
GOLANGCI := go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_VERSION)
# Keep gopls alongside golangci-lint: its warning analyzers cover required
# diagnostics outside the configured lint and go test gates.
GOPLS_VERSION := v0.23.0
GOPLS := go run golang.org/x/tools/gopls@$(GOPLS_VERSION)
MODERNIZE := go run golang.org/x/tools/gopls/internal/analysis/modernize/cmd/modernize@$(GOPLS_VERSION)
GOVULN := go run golang.org/x/vuln/cmd/govulncheck@latest
ACTIONLINT := go run github.com/rhysd/actionlint/cmd/actionlint@v1.7.12
GOVENDOR := go run github.com/purpleclay/go-overlay/cmd/govendor@v1.4.0
NIX ?= nix
DOCKER ?= docker

CONTAINER_IMAGE    ?= tturl:local
CONTAINER_SOURCE   ?= https://github.com/tantosec/tturl
CONTAINER_DESCRIPTION ?= HTTP/2 CLI for timeless timing attacks and request races
CONTAINER_VERSION  ?= devel
CONTAINER_REVISION ?= $(shell git rev-parse HEAD 2>/dev/null)
CONTAINER_TREE     ?= $(shell git diff --quiet HEAD -- 2>/dev/null && echo clean || echo dirty)

.PHONY: check check-code policy-check compatibility-check fmt lint test race \
	spellcheck spellcheck-audit spelling \
	shell-completion-test ranking-evaluate ranking-qualification-plan \
	ranking-qualify ranking-qualification-campaign-plan \
	ranking-qualification-campaign ranking-qualification-verify \
	ranking-qualification-replay vuln gopls modernize workflow-lint \
	golangci-version nix-check nix-deps nix-deps-check nix-native-check \
	release-check release-metadata-check release-prepare release-rehearse \
	release-snapshot release-verify \
	release-prepare-rollback-test \
	container-build container-check

# The complete ordinary development gate. CI runs the component sets with each
# appropriate Go version; scope package-aware checks with PKG=./pkg/...
check: policy-check compatibility-check

# Fast source gate for the pre-commit hook. The completion gate above adds the
# live vulnerability scan.
check-code: policy-check test

# Repository policy is independent of the supported Go release line.
policy-check: release-metadata-check nix-deps-check spellcheck lint gopls modernize \
	workflow-lint

# Tests and vulnerability results depend on the selected Go toolchain.
compatibility-check: vuln test

lint:
	$(GOLANGCI) fmt --diff $(PKG)
	$(GOLANGCI) run $(PKG)

spelling: spellcheck

spellcheck: .cspell/node_modules/.package-lock.json
	npm exec --offline --prefix .cspell -- \
		cspell lint --dot --no-progress --validate-directives .

spellcheck-audit: .cspell/node_modules/.package-lock.json
	node .cspell/audit-project-words.mjs tturl

.cspell/node_modules/.package-lock.json: .cspell/package.json .cspell/package-lock.json
	npm ci --ignore-scripts --prefix .cspell

# Machine-readable pin used by the CI binary installer. Keeping the version in
# one place makes a partial tool upgrade fail closed on its archived checksum.
golangci-version:
	@echo "$(GOLANGCI_VERSION)"

test:
	go test $(PKG)

# The maximum-workload replay is a sequential scale-contract test. It runs in
# the ordinary test suite; smaller tests retain race coverage of the same
# reporting and replay paths without magnifying its 50,000-operation cost.
race:
	go test -race \
		-skip '^TestAnalyseJSONReplayAtMaximumSupportedWorkload$$' $(PKG)

# Validate generated scripts with their native interpreters. This optional
# integration suite requires Bash with programmable completion, Zsh, and fish.
shell-completion-test:
	@command -v bash >/dev/null || { echo 'Bash is required' >&2; exit 1; }
	@bash --noprofile --norc -c 'type complete' >/dev/null 2>&1 || \
		{ echo 'Bash programmable completion is required' >&2; exit 1; }
	@command -v zsh >/dev/null || { echo 'Zsh is required' >&2; exit 1; }
	@command -v fish >/dev/null || { echo 'fish is required' >&2; exit 1; }
	go test ./cmd/tturl \
		-run '^TestGeneratedCompletionShellSyntax$$' -count=1

# Exploratory outlier-solver evaluation. A panel reports comparative evidence;
# it is not a release gate. Selectors are regular expressions.
ranking-evaluate:
	go test ./internal/ranking -run '^TestOutlierSolverEvaluation$$' -count=1 -v \
		-args -evaluation.panel='$(RANKING_PANEL)' \
		-evaluation.runs='$(RANKING_RUNS)' \
		-evaluation.seed='$(RANKING_SEED_START)' \
		-evaluation.solver='$(RANKING_SOLVER)' \
		-evaluation.scenario='$(RANKING_SCENARIO)' \
		-evaluation.output='$(RANKING_OUTPUT)'

# Print the versioned qualification plan and its authoritative shard list.
ranking-qualification-plan:
	go test ./internal/ranking -run '^TestOutlierSolverQualification$$' -count=1 -v \
		-args -qualification.list

# Enforcing release qualification. The plan owns its driver, fleet, scenarios,
# checks, run count, seed window, and statistical spending. A shard selector is
# an exact admitted solver ID, or "all" for a complete local run.
ranking-qualify:
	go test ./internal/ranking -run '^TestOutlierSolverQualification$$' -count=1 -v \
		-args -qualification.shard='$(RANKING_QUALIFICATION_SHARD)' \
		-qualification.output='$(RANKING_OUTPUT)'

# Describe an independent campaign without executing it. Supply exactly one of
# RANKING_CAMPAIGN_SEED_START or RANKING_CAMPAIGN_RUN_ID.
ranking-qualification-campaign-plan:
	go test ./internal/ranking -run '^TestOutlierSolverQualificationCampaign$$' -count=1 -v \
		-args -qualification-campaign.list \
		-qualification-campaign.seed='$(RANKING_CAMPAIGN_SEED_START)' \
		-qualification-campaign.run-id='$(RANKING_CAMPAIGN_RUN_ID)'

# Run one exact shard or the complete fleet on a prospectively selected seed
# window. Unlike the fixed gate, a campaign always requires canonical output.
ranking-qualification-campaign:
	go test ./internal/ranking -run '^TestOutlierSolverQualificationCampaign$$' -count=1 -v \
		-args -qualification-campaign.shard='$(RANKING_QUALIFICATION_SHARD)' \
		-qualification-campaign.seed='$(RANKING_CAMPAIGN_SEED_START)' \
		-qualification-campaign.run-id='$(RANKING_CAMPAIGN_RUN_ID)' \
		-qualification-campaign.output='$(RANKING_OUTPUT)'

# Verify one evidence file or a directory containing a complete fleet bundle.
ranking-qualification-verify:
	go test ./internal/ranking -run '^TestOutlierSolverQualificationVerify$$' -count=1 -v \
		-args -qualification-verify.path='$(RANKING_EVIDENCE_PATH)' \
		-qualification-verify.summary='$(RANKING_SUMMARY_OUTPUT)' \
		-qualification-verify.disposition='$(RANKING_DISPOSITION_OUTPUT)'

# Reproduce one exact stored trial against the matching source and plan.
ranking-qualification-replay:
	go test ./internal/ranking -run '^TestOutlierSolverQualificationReplay$$' -count=1 -v \
		-args -qualification-replay.path='$(RANKING_EVIDENCE_PATH)' \
		-qualification-replay.scenario='$(RANKING_REPLAY_SCENARIO)' \
		-qualification-replay.solver='$(RANKING_REPLAY_SOLVER)' \
		-qualification-replay.seed='$(RANKING_REPLAY_SEED)'

vuln:
	$(GOVULN) $(PKG)

# Regenerate or check the per-module dependency manifest using Go alone. The
# actual Nix package build remains an independent CI proof.
nix-deps:
	$(GOVENDOR)

nix-deps-check:
	$(GOVENDOR) --check

# Complete local composition for Nix users. Contributors and release
# maintainers do not need this target locally. CI's policy job already runs
# nix-deps-check, so its Nix job calls nix-native-check directly.
nix-check: nix-deps-check nix-native-check

# Evaluate, build, and exercise the flake. This internal component deliberately
# excludes the Go-native dependency-manifest proof composed by nix-check.
nix-native-check:
	NIX=$(NIX) ./scripts/nix-check.sh

# Prepare the version-bearing diff for a release pull request. The script starts
# from a clean tree and restores both metadata files on failure.
release-prepare:
	@test -n "$(VERSION)" || { echo 'usage: make release-prepare VERSION=X.Y.Z' >&2; exit 2; }
	./scripts/release-prepare.sh "$(VERSION)"

# Exercise release-preparation rollback in disposable Git repositories. This
# explicit integration check launches shell and Git commands; ordinary Go tests
# and `make check` deliberately do not run it.
release-prepare-rollback-test:
	./scripts/release-prepare-rollback-test.sh

# Non-mutating identity check used by the tag workflow before publication.
release-check:
	@test -n "$(VERSION)" || { echo 'usage: make release-check VERSION=X.Y.Z' >&2; exit 2; }
	git tag --list 'v*' | go run ./tools/releasemaint check "$(VERSION)"

# Keep the reviewed release version and its citation metadata in lockstep.
release-metadata-check:
	go run ./tools/releasemaint check-metadata

# Exercise release-only GoReleaser templates at a synthetic tag without
# publishing or changing the current repository's history.
release-rehearse:
	@test -n "$(VERSION)" || { echo 'usage: make release-rehearse VERSION=X.Y.Z' >&2; exit 2; }
	./scripts/release-rehearse.sh "$(VERSION)"

# Build and verify the exact checked-out release tag without publishing it.
release-verify:
	@test -n "$(VERSION)" -a -n "$(REVISION)" || { echo 'usage: make release-verify VERSION=X.Y.Z REVISION=FULL_COMMIT' >&2; exit 2; }
	./scripts/goreleaser.sh check
	./scripts/goreleaser.sh release --clean --skip=publish
	go run ./tools/releasemaint verify-dist "$(VERSION)" "$(REVISION)" dist

# Cross-build and archive the release matrix without publishing anything.
release-snapshot:
	./scripts/goreleaser.sh check
	./scripts/goreleaser.sh release --snapshot --clean --skip=publish

# Build the native tturl image. Docker is optional locally; CI owns the
# required image proof and release publication.
container-build:
	@test -n "$(CONTAINER_REVISION)" || { echo 'container revision is unavailable' >&2; exit 2; }
	$(DOCKER) build \
		--file cmd/tturl/docker/Dockerfile \
		--build-arg SOURCE="$(CONTAINER_SOURCE)" \
		--build-arg DESCRIPTION="$(CONTAINER_DESCRIPTION)" \
		--build-arg VERSION="$(CONTAINER_VERSION)" \
		--build-arg REVISION="$(CONTAINER_REVISION)" \
		--build-arg TREE="$(CONTAINER_TREE)" \
		--tag "$(CONTAINER_IMAGE)" .

# Exercise the image contract on the native architecture.
container-check: container-build
	@set -eu; \
		user="$$($(DOCKER) image inspect --format '{{.Config.User}}' "$(CONTAINER_IMAGE)")"; \
		test "$$user" = '65532:65532' || { echo "container user is $$user" >&2; exit 1; }; \
		for pair in \
			"org.opencontainers.image.source=$(CONTAINER_SOURCE)" \
			"org.opencontainers.image.revision=$(CONTAINER_REVISION)" \
			"org.opencontainers.image.version=$(CONTAINER_VERSION)" \
			"org.opencontainers.image.title=tturl" \
			"org.opencontainers.image.description=$(CONTAINER_DESCRIPTION)" \
			"org.opencontainers.image.licenses=MIT"; do \
			name="$${pair%%=*}"; want_label="$${pair#*=}"; \
			got_label="$$($(DOCKER) image inspect \
				--format "{{index .Config.Labels \"$$name\"}}" "$(CONTAINER_IMAGE)")"; \
			test "$$got_label" = "$$want_label" || { \
				echo "container label $$name is $$got_label; want $$want_label" >&2; \
				exit 1; \
			}; \
		done; \
		short_revision="$$(printf '%.12s' "$(CONTAINER_REVISION)")"; \
		dirty=''; \
		display_version='$(CONTAINER_VERSION)'; \
		if [ "$(CONTAINER_TREE)" != clean ]; then \
			dirty='-dirty'; display_version=devel; \
		fi; \
		want="tturl $${display_version} ($${short_revision}$${dirty})"; \
		got="$$($(DOCKER) run --rm "$(CONTAINER_IMAGE)" --version)"; \
		test "$$got" = "$$want" || { echo "version is $$got; want $$want" >&2; exit 1; }; \
		$(DOCKER) run --rm "$(CONTAINER_IMAGE)" --help >/dev/null; \
		if $(DOCKER) run --rm "$(CONTAINER_IMAGE)" race --trials 1 \
			https://127.0.0.1:1/ >/dev/null 2>&1; then \
			echo 'connection-failure smoke test unexpectedly succeeded' >&2; \
			exit 1; \
		fi; \
		container_id="$$($(DOCKER) create "$(CONTAINER_IMAGE)")"; \
		certificate_bundle="$$(mktemp)"; \
		license="$$(mktemp)"; \
		notices="$$(mktemp)"; \
		trap '$(DOCKER) rm -f "$$container_id" >/dev/null; rm -f "$$certificate_bundle" "$$license" "$$notices"' EXIT HUP INT TERM; \
		$(DOCKER) cp "$$container_id":/LICENSE "$$license"; \
		cmp -s LICENSE "$$license" || { \
			echo 'container has the wrong LICENSE' >&2; \
			exit 1; \
		}; \
		$(DOCKER) cp "$$container_id":/THIRD_PARTY_LICENSES "$$notices"; \
		cmp -s THIRD_PARTY_LICENSES "$$notices" || { \
			echo 'container has the wrong THIRD_PARTY_LICENSES' >&2; \
			exit 1; \
		}; \
		$(DOCKER) export "$$container_id" | \
			tar -xOf - etc/ssl/certs/ca-certificates.crt >"$$certificate_bundle"; \
		grep -q '^-----BEGIN CERTIFICATE-----$$' "$$certificate_bundle"

# gopls check reports analyzer diagnostics without returning a failing status,
# so any output is a failed gate. Resolve and compile the pinned tool first:
# go run's cold-cache download messages are acquisition output, not diagnostics.
gopls:
	@$(GOPLS) version >/dev/null
	@set -eu; diagnostics="$$(mktemp)"; packages="$$(mktemp)"; \
		trap 'rm -f "$$diagnostics" "$$packages"' EXIT HUP INT TERM; \
		go list -f '{{.Dir}}' $(PKG) >"$$packages"; \
		set --; \
		while IFS= read -r package_dir; do \
			for file in "$$package_dir"/*.go; do \
				[ ! -e "$$file" ] || set -- "$$@" "$$file"; \
			done; \
		done <"$$packages"; \
		if ! $(GOPLS) check "$$@" >"$$diagnostics" 2>&1; then \
			cat "$$diagnostics"; \
			exit 1; \
		fi; \
		if [ -s "$$diagnostics" ]; then \
			cat "$$diagnostics"; \
			exit 1; \
		fi

modernize:
	$(MODERNIZE) $(PKG)

workflow-lint:
	$(ACTIONLINT) .github/workflows/*.yaml

# fmt: apply formatting, imports, and modernisation in place.
fmt:
	$(GOLANGCI) fmt $(PKG)
	$(MODERNIZE) -fix $(PKG)

# Local-only developer targets. Absent from a clean clone; the leading `-` makes
# the include a no-op when the file is not present.
-include Makefile.local
