SHELL := /bin/bash

MODULE    := github.com/byjackchen/trade-tms-go
BIN_DIR   := bin
BINARY    := $(BIN_DIR)/tms

VERSION    ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT     ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
BUILD_DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

LDFLAGS := -s -w \
	-X $(MODULE)/internal/app.Version=$(VERSION) \
	-X $(MODULE)/internal/app.Commit=$(COMMIT) \
	-X $(MODULE)/internal/app.BuildDate=$(BUILD_DATE)

# Integration tests run against the compose stack (host-mapped ports).
ITEST_ENV := TMS_PG_HOST=127.0.0.1 TMS_PG_PORT=55432 TMS_PG_USER=tms \
	TMS_PG_PASSWORD=tms TMS_PG_DATABASE=tms TMS_REDIS_ADDR=127.0.0.1:56379

# E2E (Playwright) — host-mapped ports, bearer token shared by api/worker/ui.
# TMS_API_TOKEN is overridable; the default is a local-only dev token. The same
# value is exported to compose (so api/worker/ui agree) and to the suite (so the
# direct-API auth test and DB-truth assertions use the right credentials).
E2E_DIR        := e2e
TMS_API_TOKEN  ?= local-e2e-token
E2E_ENV := TMS_API_TOKEN=$(TMS_API_TOKEN) \
	TMS_E2E_UI_URL=http://localhost:13000 \
	TMS_E2E_API_URL=http://localhost:18080 \
	$(ITEST_ENV)

# ---------------------------------------------------------------------------
# Golden parity gate (Go engine vs reference Nautilus BacktestEngine).
# ---------------------------------------------------------------------------
# The reference Python repo (read-only) supplies Nautilus + the Sharadar cache.
# MS_REPO points at it; MS_PY is its venv interpreter. Override on the CLI if
# your checkout lives elsewhere. The harness, comparator and probe live under
# tmp/parity/ (throwaway, gitignored); the canonical script + golden fixtures
# are committed under testdata/.
MS_REPO        ?= /Users/byjackchen/codespace/trade-multi-strategies
MS_PY          ?= $(MS_REPO)/.venv/bin/python
PARITY_DIR     := tmp/parity
PARITY_SCRIPT  ?= testdata/parity/script_canonical.json
PARITY_NAU_OUT := $(PARITY_DIR)/nautilus_out
PARITY_GO_OUT  := $(PARITY_DIR)/go_out
PARITY_RUN_TS  := 2021-01-04_00-00-00

.PHONY: all build test vet fmt-check itest compose-up compose-down docker-build clean \
	e2e-install e2e itest-full e2e-seed \
	parity parity-nautilus parity-go parity-compare parity-depthwalk

all: fmt-check vet build test

build:
	mkdir -p $(BIN_DIR)
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(BINARY) ./cmd/tms

test:
	go test -race ./...

vet:
	go vet ./...

fmt-check:
	@out="$$(gofmt -l .)"; \
	if [ -n "$$out" ]; then \
		echo "gofmt: the following files need formatting:"; echo "$$out"; exit 1; \
	fi

# Bring up the base stack (postgres + redis + migrations) and run
# integration-tagged tests against it.
itest: compose-up
	$(ITEST_ENV) go test -race -tags integration -count=1 ./...

# ---------------------------------------------------------------------------
# End-to-end (Playwright) targets.
# ---------------------------------------------------------------------------

# Install the suite's npm deps + the chromium browser (one-time; the gate runs
# this ahead of time so the test run never pays the download).
e2e-install:
	cd $(E2E_DIR) && npm install
	cd $(E2E_DIR) && npx playwright install chromium

# Apply the idempotent e2e seed only when market data is empty (leaves a
# populated stack untouched). Requires the base stack (postgres) to be up.
e2e-seed:
	cd $(E2E_DIR) && npx tsx seed/seed.ts --if-empty

# Run the suite headless against an already-up stack (UI 13000 / API 18080).
# Does NOT manage compose — use `itest-full` for a self-contained run.
e2e:
	$(E2E_ENV) bash -c 'cd $(E2E_DIR) && npx playwright test'

# Full integration loop: build + bring up the entire app stack (postgres,
# redis, migrate, api, worker, ui) waiting on healthchecks, seed market data if
# empty, run the e2e suite, then tear the stack down regardless of outcome.
#
# The app containers read TMS_API_TOKEN only from .env (compose env_file). To
# stay self-contained without clobbering an operator-managed .env, we ensure a
# token line exists: if .env has no TMS_API_TOKEN= with a value, append the
# default. The api/worker/ui then all agree on the same bearer token as the
# suite ($(TMS_API_TOKEN)).
itest-full:
	@touch .env
	@if ! grep -qE '^TMS_API_TOKEN=.+' .env; then \
		echo "TMS_API_TOKEN=$(TMS_API_TOKEN)" >> .env; \
		echo "[itest-full] appended TMS_API_TOKEN to .env"; \
	fi
	docker compose --profile app up -d --build --wait
	@set -e; \
	TOKEN="$$(grep -E '^TMS_API_TOKEN=.+' .env | head -1 | cut -d= -f2-)"; \
	export TMS_API_TOKEN="$$TOKEN"; \
	echo "[itest-full] suite + stack share TMS_API_TOKEN from .env"; \
	status=0; \
	( $(ITEST_ENV) TMS_E2E_UI_URL=http://localhost:13000 TMS_E2E_API_URL=http://localhost:18080 \
		bash -c 'cd $(E2E_DIR) && npx tsx seed/seed.ts --if-empty' ) || status=$$?; \
	if [ $$status -eq 0 ]; then \
		( $(ITEST_ENV) TMS_E2E_UI_URL=http://localhost:13000 TMS_E2E_API_URL=http://localhost:18080 \
			bash -c 'cd $(E2E_DIR) && npx playwright test' ) || status=$$?; \
	fi; \
	docker compose --profile app down; \
	exit $$status

compose-up:
	docker compose up -d --build --wait postgres redis migrate

compose-down:
	docker compose down

docker-build:
	docker build -t tms:dev \
		--build-arg VERSION=$(VERSION) \
		--build-arg COMMIT=$(COMMIT) \
		--build-arg BUILD_DATE=$(BUILD_DATE) .

clean:
	rm -rf $(BIN_DIR)

# ---------------------------------------------------------------------------
# Golden parity targets.
# ---------------------------------------------------------------------------

# Run the reference Nautilus harness over the canonical script: loads the SAME
# Sharadar bars, dumps bars.json (the shared inputs) + orders/positions/account/
# equity to PARITY_NAU_OUT. Run with the reference repo's venv, cwd = MS_REPO so
# its `src` + nautilus_trader import.
parity-nautilus:
	@mkdir -p $(PARITY_NAU_OUT)
	cd $(MS_REPO) && $(MS_PY) $(CURDIR)/$(PARITY_DIR)/nautilus_parity.py \
		--script $(CURDIR)/$(PARITY_SCRIPT) \
		--out $(CURDIR)/$(PARITY_NAU_OUT)

# Run the Go engine over the canonical script + the SAME bars.json the Nautilus
# harness dumped, ZERO-COST (nautilus-compat), into PARITY_GO_OUT/<run-ts>.
parity-go:
	go run ./cmd/tms parity-backtest \
		--script $(PARITY_SCRIPT) \
		--bars $(PARITY_NAU_OUT)/bars.json \
		--runs-root $(PARITY_GO_OUT) \
		--run-ts $(PARITY_RUN_TS)

# Diff the Go run against the Nautilus dump field-by-field (prices exact after
# fixed-point, pnl/equity within a cent, counts + ordering exact).
parity-compare:
	$(MS_PY) $(PARITY_DIR)/compare_engine.py \
		--go $(PARITY_GO_OUT)/$(PARITY_RUN_TS) \
		--nautilus $(PARITY_NAU_OUT)

# Regenerate the depth-walk golden table (the small-volume fill rule the Go
# unit test asserts) by probing the Nautilus matching engine directly.
parity-depthwalk:
	cd $(MS_REPO) && $(MS_PY) $(CURDIR)/$(PARITY_DIR)/probe_depthwalk.py
	cp $(PARITY_NAU_OUT)/depthwalk.json internal/exec/testdata/depthwalk.json

# Full golden-parity gate: Nautilus side + Go side + comparator. Non-zero exit
# if the engines diverge beyond tolerance.
parity: parity-nautilus parity-go parity-compare
	@echo "[parity] golden-parity gate passed"
