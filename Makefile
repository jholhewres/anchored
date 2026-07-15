CGO_CFLAGS=-DSQLITE_ENABLE_FTS5
CGO_LDFLAGS=-lm

# macOS doesn't need -lm (it's part of the system)
UNAME_S := $(shell uname -s)
ifeq ($(UNAME_S),Darwin)
  CGO_LDFLAGS=
endif

# Single source of truth for the version. Build injects it into main.Version
# via -ldflags so the binary, plugin manifests, and goreleaser tags stay in
# lockstep. Bumping the release is `echo X.Y.Z > VERSION && make sync-version`.
VERSION := $(shell cat VERSION)
LDFLAGS := -X main.Version=$(VERSION)

# Canonical location the installer (install/install.sh) writes to, and the
# freshly built binary produced by `make build`.
INSTALL_BIN := $(HOME)/.anchored/bin/anchored
SRC_BIN     := $(CURDIR)/bin/anchored

.PHONY: build test lint clean sync-version eval sync-bin sync-bin-dry

build:
	CGO_CFLAGS="$(CGO_CFLAGS)" CGO_LDFLAGS="$(CGO_LDFLAGS)" go build -ldflags "$(LDFLAGS)" -o bin/anchored ./cmd/anchored/

test:
	CGO_CFLAGS="$(CGO_CFLAGS)" CGO_LDFLAGS="$(CGO_LDFLAGS)" go test ./... -v

# Local evaluation gates (recall, sync-safety, identity). Builds the binary and
# runs each eval against its embedded fixture; any failure exits non-zero so CI
# can gate on it.
eval: build
	./bin/anchored eval recall
	./bin/anchored eval sync-safety
	./bin/anchored eval identity

lint:
	CGO_CFLAGS="$(CGO_CFLAGS)" CGO_LDFLAGS="$(CGO_LDFLAGS)" golangci-lint run ./...

clean:
	rm -rf bin/

sync-version:
	go run ./cmd/version-sync

# Discover every installed `anchored` on this machine and overwrite it with the
# freshly built binary. Locations are found at runtime via whereis + command -v
# (so it works for any username / install layout), plus the canonical installer
# path. The repo's own bin/anchored is never a target. Paths owned by root fall
# back to sudo.
sync-bin: build
	@echo "Discovering installed anchored binaries..."
	@targets="$(INSTALL_BIN) $$(command -v anchored 2>/dev/null) \
	  $$(whereis -b anchored 2>/dev/null | cut -d: -f2-)"; \
	seen=""; found=0; \
	for t in $$targets; do \
	  [ -n "$$t" ] || continue; \
	  [ "$$t" = "$(SRC_BIN)" ] && continue; \
	  case " $$seen " in *" $$t "*) continue;; esac; \
	  seen="$$seen $$t"; \
	  [ -e "$$t" ] || continue; \
	  found=1; \
	  if cp -f "$(SRC_BIN)" "$$t" 2>/dev/null; then \
	    echo "  synced  $$t"; \
	  elif sudo cp -f "$(SRC_BIN)" "$$t"; then \
	    echo "  synced  $$t (sudo)"; \
	  else \
	    echo "  FAILED  $$t"; \
	  fi; \
	done; \
	[ "$$found" = "1" ] || echo "  no installed anchored found (nothing synced)"

# Preview which paths sync-bin would target, without copying.
sync-bin-dry:
	@targets="$(INSTALL_BIN) $$(command -v anchored 2>/dev/null) \
	  $$(whereis -b anchored 2>/dev/null | cut -d: -f2-)"; \
	seen=""; \
	for t in $$targets; do \
	  [ -n "$$t" ] || continue; \
	  [ "$$t" = "$(SRC_BIN)" ] && continue; \
	  case " $$seen " in *" $$t "*) continue;; esac; \
	  seen="$$seen $$t"; \
	  [ -e "$$t" ] && echo "  would sync  $$t" || echo "  (absent)    $$t"; \
	done
