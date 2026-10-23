SHELL := bash

BINARY   := postmarkd
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS  := -s -w -X main.version=$(VERSION)

PREFIX     ?= /usr
SYSCONFDIR ?= /etc
UNITDIR    ?= $(PREFIX)/lib/systemd/system
SYSUSERSDIR?= $(PREFIX)/lib/sysusers.d
DESTDIR    ?=

.PHONY: all build test race cover vet fmt lint vuln doccheck bench fuzz check site site-serve brand install install-sendmail uninstall docker clean

all: vet test build

build:
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o bin/$(BINARY) ./cmd/postmarkd

test:
	go test ./...

race:
	go test -race ./...

# Race tests plus the coverage gate: 90% of hand-written statements.
# Generated files (listed by tools/generated) and examples/ are excluded.
COVERMIN ?= 90
cover:
	go test -race -coverprofile=coverage.out ./...
	{ head -1 coverage.out; grep -v '^mode:' coverage.out | grep -vFf <(go run ./tools/generated ./...) | grep -v '/examples/'; } > coverage.hand.out
	go tool cover -func=coverage.hand.out | awk -v min=$(COVERMIN) '/^total:/ {sub("%","",$$3); print "coverage " $$3 "%"; if ($$3+0 < min) {print "coverage below " min "%"; exit 1}}'

vet:
	go vet ./...

fmt:
	gofmt -w .

# Benchmarks: make bench > new.txt; benchstat old.txt new.txt
BENCH ?= .
BENCHCOUNT ?= 6
bench:
	go test -run '^$$' -bench '$(BENCH)' -benchmem -count $(BENCHCOUNT) ./internal/...

# Documentation site: built into _site/ from docs/ (see site/).
site:
	cd site && go run . -out ../_site

site-serve: site
	@echo "http://localhost:8000"
	cd _site && python3 -m http.server 8000

# Regenerate logos, banners and the social card (needs Node and Chromium).
brand:
	cd assets/brand/src && npm install --no-audit --no-fund && npm run build

lint: vet
	@test -z "$$(gofmt -l .)" || (echo "gofmt needed:"; gofmt -l .; exit 1)
	golangci-lint run ./...

vuln:
	go run golang.org/x/vuln/cmd/govulncheck@v1.9.0 ./...

doccheck:
	go run ./tools/doccheck ./...

# Run every fuzz target for FUZZTIME each.
FUZZTIME ?= 10s
fuzz:
	@set -e; for pkg in $$(go list ./...); do \
		for t in $$(go test -list '^Fuzz' $$pkg | grep '^Fuzz' || true); do \
			echo "== $$pkg $$t"; \
			go test -run '^$$' -fuzz "^$$t$$" -fuzztime $(FUZZTIME) -fuzzminimizetime 5s $$pkg; \
		done; \
	done

# Everything CI checks, minus the container and site builds.
check: lint cover doccheck vuln fuzz
	go test -run '^$$' -bench . -benchtime 1x ./...
	goreleaser check

install: build
	install -Dm0755 bin/$(BINARY) $(DESTDIR)$(PREFIX)/bin/$(BINARY)
	install -Dm0644 deploy/systemd/postmarkd.service $(DESTDIR)$(UNITDIR)/postmarkd.service
	install -Dm0644 deploy/systemd/postmarkd.socket $(DESTDIR)$(UNITDIR)/postmarkd.socket
	install -Dm0644 deploy/systemd/postmarkd-smtp.socket $(DESTDIR)$(UNITDIR)/postmarkd-smtp.socket
	install -Dm0644 deploy/systemd/postmarkd.sysusers $(DESTDIR)$(SYSUSERSDIR)/postmarkd.conf
	@if [ ! -e $(DESTDIR)$(SYSCONFDIR)/postmarkd/postmarkd.conf ]; then \
		install -Dm0640 deploy/postmarkd.conf $(DESTDIR)$(SYSCONFDIR)/postmarkd/postmarkd.conf; \
	else echo "keeping existing $(SYSCONFDIR)/postmarkd/postmarkd.conf"; fi
	@if [ -z "$(DESTDIR)" ] && command -v systemd-sysusers >/dev/null; then \
		systemd-sysusers $(SYSUSERSDIR)/postmarkd.conf && \
		chgrp postmarkd $(SYSCONFDIR)/postmarkd/postmarkd.conf && \
		systemctl daemon-reload; fi
	@if [ "$(PREFIX)" != "/usr" ]; then \
		sed -i 's|/usr/bin/postmarkd|$(PREFIX)/bin/postmarkd|' $(DESTDIR)$(UNITDIR)/postmarkd.service; fi

# Make every program that calls sendmail(8) deliver through postmarkd.
install-sendmail:
	@if [ -e $(DESTDIR)/usr/sbin/sendmail ] && [ "$$(readlink -f $(DESTDIR)/usr/sbin/sendmail)" != "$(PREFIX)/bin/$(BINARY)" ]; then \
		echo "/usr/sbin/sendmail already exists (another MTA?). Remove it first."; exit 1; fi
	ln -sf $(PREFIX)/bin/$(BINARY) $(DESTDIR)/usr/sbin/sendmail

uninstall:
	rm -f $(DESTDIR)$(PREFIX)/bin/$(BINARY) \
		$(DESTDIR)$(UNITDIR)/postmarkd.service \
		$(DESTDIR)$(UNITDIR)/postmarkd.socket \
		$(DESTDIR)$(UNITDIR)/postmarkd-smtp.socket \
		$(DESTDIR)$(SYSUSERSDIR)/postmarkd.conf

docker:
	docker build --build-arg VERSION=$(VERSION) -t postmarkd:$(VERSION) .

clean:
	rm -rf bin dist _site coverage.out coverage.hand.out
