# basalt developer tasks. Parallelism is kept modest (-p 4).
GO      ?= go
PYTHON  ?= python3
PKGS    := ./...

.PHONY: build test race lint psql-test python-test crash-test slt difftest bench bench-tpcb bench-analytics clean

build:
	CGO_ENABLED=0 $(GO) build -trimpath -o bin/basalt ./cmd/basalt

test:
	$(GO) test -p 4 $(PKGS)

race:
	$(GO) test -p 4 -race -count=1 $(PKGS)

lint:
	@test -z "$$(gofmt -l . | tee /dev/stderr)" || (echo "gofmt needed on the files above"; exit 1)
	$(GO) vet $(PKGS)
	$(GO) run honnef.co/go/tools/cmd/staticcheck@latest $(PKGS)

# Real clients.
psql-test:
	test/psql/run.sh

python-test:
	$(PYTHON) test/python/test_psycopg.py

# Kills the server process at random points under a write workload.
crash-test:
	BASALT_CRASH_TEST=1 $(GO) test -count=1 -run TestCrashKill -v ./test/crash/

# SQLite's sqllogictest corpus (downloaded to testdata/, not committed).
slt:
	BASALT_SLT=1 $(GO) test -count=1 -timeout 60m -run TestSQLLogic -v ./test/slt/

# Random queries compared with sqlite3 on the same data.
difftest:
	BASALT_DIFFTEST=1 $(GO) test -count=1 -run TestDifferential -v ./test/difftest/

bench: bench-tpcb bench-analytics

bench-tpcb: build
	$(GO) run ./bench/tpcb -basalt bin/basalt

bench-analytics: build
	$(GO) run ./bench/analytics -basalt bin/basalt

clean:
	rm -rf bin coverage.out
