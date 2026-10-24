# Give an application image a working /usr/sbin/sendmail (PHP mail(), cron,
# mailx, git send-email, …) that delivers through a postmarkd sidecar.
FROM php:8.3-apache

COPY --from=ghcr.io/rossedman/postmarkd:0.1 /usr/bin/postmarkd /usr/sbin/sendmail

# Where the sidecar is: a shared socket volume, or host:port.
ENV POSTMARKD_ADDR=/run/postmarkd/postmarkd.sock
# PHP: mail() calls "sendmail -t -i" by default, which postmarkd understands.

# Run as the image's unprivileged web user. Docker lets non-root processes
# bind port 80; on Kubernetes, move Apache to a high port (ports.conf) or set
# the net.ipv4.ip_unprivileged_port_start sysctl.
USER www-data
