CGO_CFLAGS=-DSQLITE_ENABLE_FTS5
CGO_LDFLAGS=-lm

# macOS doesn't need -lm (it's part of the system)
UNAME_S := $(shell uname -s)
ifeq ($(UNAME_S),Darwin)
  CGO_LDFLAGS=
endif

.PHONY: build test lint clean

build:
	CGO_CFLAGS="$(CGO_CFLAGS)" CGO_LDFLAGS="$(CGO_LDFLAGS)" go build -o bin/anchored ./cmd/anchored/

test:
	CGO_CFLAGS="$(CGO_CFLAGS)" CGO_LDFLAGS="$(CGO_LDFLAGS)" go test ./... -v

lint:
	CGO_CFLAGS="$(CGO_CFLAGS)" CGO_LDFLAGS="$(CGO_LDFLAGS)" golangci-lint run ./...

clean:
	rm -rf bin/
