# Give an application image a working /usr/sbin/sendmail (PHP mail(), cron,
# mailx, git send-email, …) that delivers through a postmarkd sidecar.
FROM php:8.3-apache

COPY --from=ghcr.io/rossedman/postmarkd:0.1 /usr/bin/postmarkd /usr/sbin/sendmail

# Where the sidecar is: a shared socket volume, or host:port.
ENV POSTMARKD_ADDR=/run/postmarkd/postmarkd.sock
# PHP: mail() calls "sendmail -t -i" by default, which postmarkd understands.
