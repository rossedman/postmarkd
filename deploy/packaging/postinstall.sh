#!/bin/sh
# Create the postmarkd user and reload unit files. The service is not
# started automatically: add a server token to /etc/postmarkd/server-token first.
set -e
if command -v systemd-sysusers >/dev/null 2>&1; then
    systemd-sysusers /usr/lib/sysusers.d/postmarkd.conf || true
fi
if [ -f /etc/postmarkd/postmarkd.conf ]; then
    chgrp postmarkd /etc/postmarkd/postmarkd.conf 2>/dev/null || true
fi
if command -v systemctl >/dev/null 2>&1; then
    systemctl daemon-reload || true
fi
