#!/bin/sh
# Create the postmarkd user and reload unit files. The service is not
# started automatically: add a server token to /etc/postmarkd/server-token first.
set -e
if command -v systemd-sysusers >/dev/null 2>&1; then
    systemd-sysusers /usr/lib/sysusers.d/postmarkd.conf || true
fi
if ! getent passwd postmarkd >/dev/null 2>&1 && command -v useradd >/dev/null 2>&1; then
    nologin=/bin/false
    for f in /usr/sbin/nologin /sbin/nologin; do [ -x "$f" ] && nologin=$f && break; done
    useradd --system --user-group --no-create-home --home-dir / --shell "$nologin" \
        --comment "postmarkd mail relay" postmarkd || true
fi
if [ -f /etc/postmarkd/postmarkd.conf ] && getent group postmarkd >/dev/null 2>&1; then
    chgrp postmarkd /etc/postmarkd/postmarkd.conf || true
fi
if [ -d /run/systemd/system ] && command -v systemctl >/dev/null 2>&1; then
    systemctl daemon-reload || true
fi
