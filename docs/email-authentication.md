# Email authentication: DKIM, SPF and DMARC

Mailbox providers decide whether your mail is genuine by checking three DNS
based mechanisms. Since 2024 Gmail and Yahoo **require** them for anyone
sending in volume, and they help everyone else stay out of spam. postmarkd
can't fix DNS for you, but `postmarkd check-domain` tells you exactly what is
missing.

## The short version

| Mechanism | What it proves | With Postmark you need |
| --- | --- | --- |
| **DKIM** | The message was signed by your domain and not altered. | A TXT record Postmark gives you (`<selector>._domainkey.yourdomain.com`). Postmark signs every message. |
| **SPF** | The sending server may send for the *envelope sender* (Return-Path) domain. | A CNAME `pm-bounces.yourdomain.com → pm.mtasv.net` and setting it as the domain's custom Return-Path in Postmark. Postmark's servers are then covered and the Return-Path is in your domain. |
| **DMARC** | Tells receivers to require that DKIM or SPF passes **for the domain in the From: address** ("alignment"), and what to do otherwise. Sends you reports. | A TXT record at `_dmarc.yourdomain.com`. |

With DKIM set up, your mail passes DMARC through DKIM alignment. The custom
Return-Path adds SPF alignment as a second, independent pass.

You don't need to change your domain's own SPF record (`v=spf1 …` on
`yourdomain.com`) for Postmark, because SPF is checked against the
Return-Path domain, which Postmark controls.

## Check your domain

```sh
postmarkd check-domain yourdomain.com
```

```
Email authentication for yourdomain.com

  [INFO] DKIM                         selector unknown, so DKIM was not checked
                                      Pass -account-token (Postmark → Account → API Tokens) or -dkim-selector …
  [WARN] Return-Path (SPF alignment)  no CNAME pm-bounces.yourdomain.com → pm.mtasv.net
                                      Create CNAME pm-bounces.yourdomain.com pointing to pm.mtasv.net …
  [FAIL] DMARC                        no DMARC record at _dmarc.yourdomain.com. Gmail and Yahoo require one for bulk senders.
                                      Create TXT _dmarc.yourdomain.com with value:
                                      v=DMARC1; p=none; rua=mailto:<reports address>
  [INFO] SPF (yourdomain.com)         no SPF record. Not needed for Postmark, which uses its own Return-Path domain.
```

Give it a Postmark **account** token (Postmark → Account → API Tokens; this
is different from the server token postmarkd sends with) and it looks up
your domain in Postmark, checks DKIM with the right selector, and prints the
exact DKIM record value to create:

```sh
POSTMARK_ACCOUNT_TOKEN=… postmarkd check-domain yourdomain.com
```

The account token is only used by this command; the daemon never needs it.
The command exits `1` when a check fails, so you can run it in CI or a
monitoring job.

## Setting it up

1. **Add the domain in Postmark** (Sender Signatures → Domains). Postmark
   shows a DKIM TXT record and a Return-Path CNAME.
2. **Create both DNS records**, then click *Verify* for each in Postmark.
3. **Add DMARC**, starting in monitoring mode:
   ```
   _dmarc.yourdomain.com  TXT  "v=DMARC1; p=none; rua=mailto:<reports address>"
   ```
   Postmark's free [DMARC monitoring](https://dmarc.postmarkapp.com) gives
   you a report address and weekly digests showing who sends mail as your
   domain.
4. **Tighten DMARC** once the reports show all your legitimate mail
   passing (including other services such as Google Workspace or your CRM):
   `p=quarantine`, then `p=reject`. This stops others from spoofing your domain.
5. Re-run `postmarkd check-domain` until everything passes.

## Other things that affect delivery

- **Send from a verified domain.** Postmark rejects mail from senders it
  hasn't verified. Set `POSTMARKD_DEFAULT_FROM` to an address on your
  verified domain so cron and system mail don't fail.
- **Use the right stream.** Keep marketing mail on a broadcast stream so it
  never affects receipts and password resets. See
  [message streams](sending.md#message-streams).
- **Unsubscribe headers.** Bulk senders must support one-click unsubscribe
  (`List-Unsubscribe` and `List-Unsubscribe-Post`). Postmark can manage
  unsubscribes for broadcast streams; if you handle them yourself, postmarkd
  forwards these headers from SMTP and sendmail messages.
