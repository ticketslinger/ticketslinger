# ticketslinger
The worlds greatest open-source ticket selling platform.

## Run it locally

```sh
make dev-db   # PostgreSQL 18 on localhost:54329, roles ts_owner and ts_app
make test     # Go tests (with -race), then bun test
```

`make dev-db-down` throws the database away.

## Run it on your own server (Docker Compose)

One Linux box, Docker, and a reverse proxy you already trust. Compose runs Postgres, applies the migrations, then starts the API, the storefront and the dashboard.

```sh
cp deploy/.env.example deploy/.env    # fill it in; the comments say what each line is
docker compose -f deploy/compose.yaml up -d --build
```

The three services use host networking and listen on `127.0.0.1` only (API 3000, storefront 3001, dashboard 3002). Your reverse proxy on the same machine terminates TLS, sends each hostname to its port, and sets `X-Real-IP` to the visitor's address. That header is how rate limits tell buyers apart, and it's only trusted from the loopback peer. Host networking works on Linux only, so Docker Desktop on macOS or Windows won't run this file.

Set the database passwords before the first `up`: the roles are created then, once. Postgres is published on `127.0.0.1:5432` and nowhere else.

## Selling with your own Stripe account (self-hosting)

Running ticketslinger yourself? Buyers can pay straight into your own Stripe account, and no platform fee comes off the top. There's nothing for your organizers to connect: set four variables and paid tickets can go on sale.

```sh
TS_PAYMENTS=direct
TS_STRIPE_SECRET_KEY=sk_live_...        # or sk_test_ while you try it
TS_STRIPE_PUBLISHABLE_KEY=pk_live_...
TS_STRIPE_WEBHOOK_SECRET=whsec_...      # from the endpoint below
```

In Stripe, go to Developers → Webhooks → Add destination, pick events from **Your account**, set the API version to `2026-09-30.endive`, and choose `payment_intent.succeeded`, `refund.created`, `refund.updated`, `refund.failed` and `charge.refunded`. The URL is `https://<your API host>/v1/webhooks/stripe`, and the signing secret it gives you is your `TS_STRIPE_WEBHOOK_SECRET`. Keep the platform-fee variables (`TS_PLATFORM_FEE_BP`, `TS_PLATFORM_FEE_CENTS`) unset; the server won't start with a fee in this mode.

Pick your payments mode before you sell your first ticket. Switch later and refunds of the payments you took before can get stranded.

## Sending email (self-hosting)

Buyers get their tickets by email, so once `TS_PUBLIC_URL` points anywhere but your own machine, the server won't start until you've told it how to send. Pick one with `TS_MAIL`:

- `TS_MAIL=resend` sends through Resend, with your key in `TS_RESEND_API_KEY` (it starts `re_`).
- `TS_MAIL=smtp` sends through your own mail server.
- `TS_MAIL=log` prints each email instead of sending it. It only works on a local `http://` URL, which is where it's the default.

Whichever you pick, set `TS_MAIL_FROM` to an address your provider will actually send as. Leave it out and you get `TicketSlinger <tickets@ticketslinger.com>`, which your relay will almost certainly refuse.

```sh
TS_MAIL=smtp
TS_MAIL_FROM="Box Office <tickets@example.com>"
TS_SMTP_HOST=smtp.example.com
TS_SMTP_PORT=587               # the default; 465 means implicit TLS
TS_SMTP_USERNAME=tickets@example.com   # set both or neither
TS_SMTP_PASSWORD=...
```

TLS is required: STARTTLS, or implicit TLS on port 465. A server that only talks plaintext never sees your password or a buyer's ticket link. Nothing goes out until TLS is on, and the certificate has to check out too.
