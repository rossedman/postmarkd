BINARY   := postmarkd
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS  := -s -w -X main.version=$(VERSION)

PREFIX     ?= /usr
SYSCONFDIR ?= /etc
UNITDIR    ?= $(PREFIX)/lib/systemd/system
SYSUSERSDIR?= $(PREFIX)/lib/sysusers.d
DESTDIR    ?=

.PHONY: all build test race vet fmt lint bench site site-serve brand install install-sendmail uninstall docker clean

all: vet test build

build:
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o bin/$(BINARY) ./cmd/postmarkd

test:
	go test ./...

race:
	go test -race ./...

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
	rm -rf bin dist _site
