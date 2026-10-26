# malxchange development tasks. Everything runs from the repo root;
# compose files live in deploy/ and read deploy/.env (make dev-up seeds
# a throwaway one from deploy/env.example if missing).

COMPOSE      = docker compose -f deploy/docker-compose.yaml
COMPOSE_DEV  = $(COMPOSE) -f deploy/compose.override.yaml
COMPOSE_TEST = $(COMPOSE) -f deploy/compose.test.yaml
# deploy/.env is gitignored, so a fresh worktree lacks it — and the
# seeding rule below would mint random creds that don't match the
# primary checkout's running dev stack. Targets that merely source it
# refuse with the fix instead (dev-up/test-profile-up keep the seeding:
# they boot their own stack with whatever they seed).
ENV_GUARD = @test -f deploy/.env || { echo "deploy/.env missing (worktree checkout?) — cp it from the primary checkout; a seeded one has random creds that won't match the running dev stack" >&2; exit 1; }

.PHONY: help dev-up dev-down dev-env test test-integration test-chain-e2e member-e2e admin-e2e test-profile-up test-profile-down test-profile-probe it-db-leftovers lint lint-guard-test build-images installer-check hooks

help:
	@echo "dev-up            start postgres+redpanda (loopback ports) for local dev"
	@echo "dev-down          stop dev infra"
	@echo "hooks             install the shared git hooks (main-checkout commit guard)"
	@echo "test              unit tests"
	@echo "test-integration  integration tests against dev infra (boot it first)"
	@echo "test-chain-e2e    chain E2E against the test profile (boot it first)"
	@echo "member-e2e        Playwright member journeys through the Caddy edge (issue #51)"
	@echo "admin-e2e         Playwright admin journeys through the Caddy edge (issue #56)"
	@echo "test-profile-up   boot the test profile (simnet chains + EVM devnet + tronstub)"
	@echo "test-profile-down stop the test profile"
	@echo "test-profile-probe health-check the chain nodes of the test profile"
	@echo "it-db-leftovers    audit dev Postgres for throwaway IT litter (read-only)"
	@echo "lint              go vet + gofmt + env-doc drift + checks"
	@echo "lint-guard-test   pin the fixed-name guard's verdicts (rejects fixed names, accepts mint forms)"
	@echo "build-images      build all service images"
	@echo "installer-check   syntax-check the installer"

test-chain-e2e:
	$(ENV_GUARD)
	@set -a; . deploy/.env; set +a; \
	MAL_TEST_BTCD_USER=$${MAL_BTCD_RPCUSER:-malxchange} MAL_TEST_BTCD_PASS=$${MAL_BTCD_RPCPASSWORD} \
	MAL_TEST_LTCD_USER=$${MAL_LTCD_RPCUSER:-malxchange} MAL_TEST_LTCD_PASS=$${MAL_LTCD_RPCPASSWORD} \
	go test -count=1 -timeout 300s -tags chain_e2e ./internal/wallet/chain/...

# Member E2E (spec #24, issue #51): boots the test+e2e profiles (simnet
# chains, mail sink, web container, Caddy edge) and runs the Playwright
# suites in apps/web. The browser reaches the app through the edge at
# https://exchange.example.com (mapped inside Chromium), so api and
# WebSocket are same-origin as in production.
member-e2e:
	$(ENV_GUARD)
	@set -a; . deploy/.env; set +a; \
	$(COMPOSE_TEST) --profile test --profile e2e up -d --build; \
	cd apps/web && npx playwright test

# Admin E2E (spec #25, issue #56): boots the test+e2e profiles (the
# admin app + admin-api behind the Caddy edge) and runs the Playwright
# suites in apps/admin. The browser reaches the app through the edge at
# https://admin.exchange.example.com (mapped inside Chromium), so the
# REST surface is same-origin exactly as in production. The first
# Employee can only ever be bootstrapped once per database: the first
# run creates it here through the installer's one-time-credential RPC
# and hands the password to the suite, which rotates it in the browser
# and stores the rotated credentials in
# apps/admin/e2e/.admin-state.json (gitignored) for later runs.
admin-e2e:
	$(ENV_GUARD)
	@set -a; . deploy/.env; set +a; \
	$(COMPOSE_TEST) --profile test --profile e2e up -d --build; \
	boot_email="$${MAL_ADMIN_BOOTSTRAP_EMAIL:-admin@exchange.example.com}"; \
	boot_pw=$$(openssl rand -hex 12); \
	boot_rc=0; \
	boot_out="$$(printf '{"email":"%s","password":"%s"}' "$$boot_email" "$$boot_pw" | $(COMPOSE_TEST) exec -T admin-api /app -bootstrap-first-employee 2>&1)" || boot_rc=$$?; \
	case "$$boot_out" in \
	  *bootstrapped*) \
	    echo "bootstrapped the first Employee ($$boot_email) for the admin e2e journey"; \
	    echo "$$boot_pw" > apps/admin/e2e/.bootstrap-password; \
	    rm -f apps/admin/e2e/.admin-state.json; \
	    echo "one-time password saved to apps/admin/e2e/.bootstrap-password (gitignored)";; \
	  *exists*) \
	    [ -f apps/admin/e2e/.admin-state.json ] || { \
	      echo "the first Employee already exists but apps/admin/e2e/.admin-state.json is missing:" >&2; \
	      echo "recreate the stack volumes, or export E2E_BOOTSTRAP_EMAIL/E2E_BOOTSTRAP_PASSWORD" >&2; exit 1; } ;; \
	  *) echo "bootstrap RPC failed (rc=$$boot_rc): $$boot_out" >&2; exit 1;; \
	esac; \
	cd apps/admin && npx playwright test

# Health-check the test-profile chain nodes: each must answer its
# canonical "where am I" RPC. Run before debugging anything chain-side —
# a node that latched (see deploy/README "Chain test-profile gotchas")
# answers garbage until recreated.
test-profile-probe:
	$(ENV_GUARD)
	@set -a; . deploy/.env; set +a; \
	echo "btcd:" && curl -fsSk -H 'Content-Type: application/json' -u "$${MAL_BTCD_RPCUSER:-malxchange}:$$MAL_BTCD_RPCPASSWORD" --data-binary '{"jsonrpc":"1.0","id":"probe","method":"getblockcount","params":[]}' https://127.0.0.1:18556 && \
	echo "ltcd:" && curl -fsSk -H 'Content-Type: application/json' -u "$${MAL_LTCD_RPCUSER:-malxchange}:$$MAL_LTCD_RPCPASSWORD" --data-binary '{"jsonrpc":"1.0","id":"probe","method":"getblockcount","params":[]}' https://127.0.0.1:19556 && \
	echo "anvil:" && curl -fsS -H 'Content-Type: application/json' --data-binary '{"jsonrpc":"2.0","id":"probe","method":"eth_blockNumber","params":[]}' http://127.0.0.1:8545 && \
	echo "all chain nodes answering"

deploy/.env:
	@echo "no deploy/.env found — seeding a throwaway dev one (DO NOT use in production)"
	@sed -e 's/replace-me/'"$$(openssl rand -hex 16)"'/g' deploy/env.example > deploy/.env
	@chmod 600 deploy/.env

dev-up: deploy/.env
	$(COMPOSE_DEV) up -d --wait postgres redpanda
	$(COMPOSE_DEV) run --rm redpanda-setup

dev-down:
	$(COMPOSE_DEV) down

test:
	go test ./...

# Scoped runs share the target's env plumbing: callers only vary the
# scope (issue #68), e.g. `make test-integration PKG=./internal/exchange/`.
PKG ?= ./internal/... ./cmd/breakglass

# Integration tests talk to the dev infra over loopback; the superuser
# credential comes from deploy/.env (same one the compose init uses).
# The break-glass CLI's per-command suite (cmd/breakglass, issue #55)
# rides along — it needs Postgres and nothing else.
test-integration:
	$(ENV_GUARD)
	set -a; . deploy/.env; set +a; \
	MAL_TEST_PG_HOST=127.0.0.1 MAL_TEST_PG_SUPERUSER=$${MAL_PG_SUPERUSER} \
	MAL_TEST_PG_SUPERUSER_PASSWORD=$${MAL_PG_SUPERUSER_PASSWORD} \
	MAL_TEST_KAFKA_SEEDS=127.0.0.1:9092 \
	MAL_TEST_KAFKA_USER=$${MAL_KAFKA_USER} MAL_TEST_KAFKA_PASSWORD=$${MAL_KAFKA_PASSWORD} \
	go test -count=1 -timeout 240s -tags integration -p 1 $(PKG)

test-profile-up: deploy/.env
	$(COMPOSE_TEST) --profile test up -d --wait

test-profile-down:
	$(COMPOSE_TEST) --profile test down

# Audit the dev Postgres for throwaway IT litter: databases and the it_
# roles of the it stack. Cleanups fire on normal exit only, so a killed
# run leaks; this lists, it never drops.
it-db-leftovers:
	$(ENV_GUARD)
	@set -a; . deploy/.env; set +a; \
	$(COMPOSE_DEV) exec -T postgres psql -U "$${MAL_PG_SUPERUSER}" -d postgres -tAc \
	  "SELECT 'db: ' || datname FROM pg_database WHERE datname LIKE 'malxchange\_it%' ESCAPE '\' UNION ALL SELECT 'role: ' || rolname FROM pg_roles WHERE rolname LIKE 'it\_%' ESCAPE '\'"

lint:
	go vet ./...
	@out=$$(gofmt -l cmd internal); if [ -n "$$out" ]; then echo "gofmt needed: $$out"; exit 1; fi
	@hits=$$(grep -rnF 'Status()[' --include='*.go' cmd internal); if [ -n "$$hits" ]; then \
		echo "positional Status() index — map order is random; select by market (docs/agents/review-checklist.md):"; echo "$$hits"; exit 1; fi
	@hits=$$( { grep -rnE '(DROP|CREATE) DATABASE (IF EXISTS )?malxchange_[a-z0-9]+' --include='*_test.go' internal; \
	           grep -rnE '"malxchange_it[a-z0-9_]*"' --include='*_test.go' internal | grep -vE 'malxchange_it[a-z]*(_" \+ hex|" \+ variant)'; } ); if [ -n "$$hits" ]; then \
		echo "fixed IT database name — mint a per-binary throwaway (docs/agents/review-checklist.md):"; echo "$$hits"; exit 1; fi
	@hits=$$(grep -rnE 'anvilSend\(t, [A-Za-z0-9_.]+, [A-Za-z0-9_.]+, nil, big\.' --include='*.go' internal); if [ -n "$$hits" ]; then \
		echo "nil-To value send — shapes a contract-creation tx anvil accepts but never mines; pass an explicit recipient (docs/agents/review-checklist.md):"; echo "$$hits"; exit 1; fi
	@{ grep -rhoE '\$$\{MAL_[A-Z0-9_]+' deploy/*.yaml 2>/dev/null | sed 's/\$${//'; \
	   grep -rhoE '(os\.Getenv|EnvDefault)\("MAL_[A-Z0-9_]+' --include='*.go' cmd internal | grep -oE 'MAL_[A-Z0-9_]+'; } \
	  | grep -vE 'MAL_(TEST_|PG_|KAFKA_|HTTP_|UNSET_)' | sort -u > /tmp/mal_wanted.$$$$; \
	 grep -hoE '^MAL_[A-Z0-9_]+' deploy/env.example | sort -u > /tmp/mal_have.$$$$; \
	 missing=$$(comm -23 /tmp/mal_wanted.$$$$ /tmp/mal_have.$$$$); rm -f /tmp/mal_wanted.$$$$ /tmp/mal_have.$$$$; \
	 if [ -n "$$missing" ]; then echo "operator-tunable MAL_* vars absent from deploy/env.example:"; echo "$$missing"; exit 1; fi
	@hits=$$(grep -rnE "['\`]/api/" apps/web/src --include='*.ts' --include='*.vue' 2>/dev/null); if [ -n "$$hits" ]; then \
		echo "apps/web hardcodes the /api prefix — useApi's base URL already carries it, so the fetch 404s on /api/api (docs/agents/review-checklist.md):"; echo "$$hits"; exit 1; fi
	@hits=$$(grep -rn "memberCall(" apps/admin/e2e 2>/dev/null | grep "'/api/"); if [ -n "$$hits" ]; then \
		echo "admin e2e memberCall paths carry /api — MEMBER_API already ends in it, so the request hits /api/api (docs/agents/review-checklist.md):"; echo "$$hits"; exit 1; fi
	@hits=$$(grep -n "healthz" deploy/docker-compose.yaml deploy/compose.test.yaml 2>/dev/null | grep -v readyz); if [ -n "$$hits" ]; then \
		echo "compose healthchecks must probe /readyz, never /healthz — liveness answers OK unconditionally (docs/agents/review-checklist.md):"; echo "$$hits"; exit 1; fi
	@if ! out=$$(cd deploy && docker compose -f docker-compose.yaml config -q 2>&1 && docker compose -f docker-compose.yaml -f compose.test.yaml config -q 2>&1); then \
		echo "compose overlay invalid — a broken compose file blocks every make target that boots the stack:"; echo "$$out"; exit 1; fi

# Ticket #67: pin the fixed-name guard's verdicts. The guard's failure
# mode is silent permissiveness — a loosened exclusion keeps CI green
# while the rule dies — so this target re-proves both directions
# against the real `lint` pipeline (not a copied regex): the two
# forbidden forms must trip it, each arm named, and the two accepted
# mint forms must pass it. The scratch file is removed on every exit
# (trap), so `git status` stays clean on pass and fail.
lint-guard-test:
	@scratch=internal/exchange/lint_guard_scratch_test.go; \
	trap 'rm -f "$$scratch"' EXIT INT TERM; \
	printf 'package exchange\n\nvar _ = "CREATE DATABASE malxchange_foo"\n\nvar _ = "malxchange_ite"\n' > "$$scratch"; \
	out=$$($(MAKE) lint 2>&1); st=$$?; \
	if [ $$st -eq 0 ]; then echo "lint-guard-test: guard went permissive — negative fixture did not trip 'make lint'"; exit 1; fi; \
	echo "$$out" | grep -q 'CREATE DATABASE malxchange_foo' || { echo "lint-guard-test: CREATE/DROP-DATABASE arm no longer rejects fixed names"; exit 1; }; \
	echo "$$out" | grep -q '"malxchange_ite"' || { echo "lint-guard-test: quoted-literal arm no longer rejects bare fixed names"; exit 1; }; \
	printf 'package exchange\n\nimport "encoding/hex"\n\nfunc lintGuardMintProbe(variant string, suffix []byte) string {\n\ta := "malxchange_itx_" + hex.EncodeToString(suffix)\n\tb := "malxchange_it" + variant + "_" + hex.EncodeToString(suffix)\n\treturn a + b\n}\n' > "$$scratch"; \
	if ! out=$$($(MAKE) lint 2>&1); then echo "lint-guard-test: accepted mint form trips the guard (over-fire):"; echo "$$out"; exit 1; fi; \
	echo "lint-guard-test: guard rejects both fixed-name forms, accepts both mint forms"

build-images:
	cd deploy && docker compose -f docker-compose.yaml build

installer-check:
	bash -n install/install.sh && echo "installer syntax OK"

# Retro (#23 session): agents commit in their own worktrees; the main
# checkout hosts the shared spec-* integration branches. The pre-commit
# hook blocks stray commits there (MAL_ALLOW_MAIN_COMMIT=1 overrides;
# ff-only merges create no commit and pass untouched).
hooks:
	@set -e; hook="$$(git rev-parse --git-common-dir)/hooks/pre-commit"; \
	printf '%s\n' \
	  '#!/bin/sh' \
	  '# Installed by `make hooks`. Agents commit in their own worktrees;' \
	  '# the main checkout hosts the shared spec-* integration branches.' \
	  '# Stray commits there pollute every sibling worktree that merges the tip.' \
	  'branch=$$(git branch --show-current)' \
	  'case "$$branch" in' \
	  'spec-*)' \
	  '  if [ "$$(git rev-parse --git-dir)" = "$$(git rev-parse --git-common-dir)" ] && [ -z "$${MAL_ALLOW_MAIN_COMMIT:-}" ]; then' \
	  '    echo "main checkout on $$branch: commit in a worktree, or set MAL_ALLOW_MAIN_COMMIT=1" >&2' \
	  '    exit 1' \
	  '  fi' \
	  '  ;;' \
	  'esac' >"$$hook"; \
	chmod +x "$$hook"; \
	echo "installed $$hook"
