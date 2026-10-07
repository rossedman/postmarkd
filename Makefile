BINARY   := postmarkd
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS  := -s -w -X main.version=$(VERSION)

PREFIX     ?= /usr
SYSCONFDIR ?= /etc
UNITDIR    ?= $(PREFIX)/lib/systemd/system
SYSUSERSDIR?= $(PREFIX)/lib/sysusers.d
DESTDIR    ?=

.PHONY: all build test race vet fmt lint install uninstall clean

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

lint: vet
	@test -z "$$(gofmt -l .)" || (echo "gofmt needed:"; gofmt -l .; exit 1)

install: build
	install -Dm0755 bin/$(BINARY) $(DESTDIR)$(PREFIX)/bin/$(BINARY)
	install -Dm0644 deploy/systemd/postmarkd.service $(DESTDIR)$(UNITDIR)/postmarkd.service
	install -Dm0644 deploy/systemd/postmarkd.socket $(DESTDIR)$(UNITDIR)/postmarkd.socket
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

uninstall:
	rm -f $(DESTDIR)$(PREFIX)/bin/$(BINARY) \
		$(DESTDIR)$(UNITDIR)/postmarkd.service \
		$(DESTDIR)$(UNITDIR)/postmarkd.socket \
		$(DESTDIR)$(SYSUSERSDIR)/postmarkd.conf

clean:
	rm -rf bin dist
