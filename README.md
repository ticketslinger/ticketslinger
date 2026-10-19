# ticketslinger
The worlds greatest open-source ticket selling platform.

## Run it locally

```sh
make dev-db   # PostgreSQL 18 on localhost:54329, roles ts_owner and ts_app
make test     # Go tests (with -race), then bun test
```

`make dev-db-down` throws the database away.

## Selling with your own Stripe account (self-hosting)

Running ticketslinger yourself? Buyers can pay straight into your own Stripe account, and no platform fee comes off the top. There's nothing for your organizers to connect: set four variables and paid tickets can go on sale.

```sh
TS_PAYMENTS=direct
TS_STRIPE_SECRET_KEY=sk_live_...        # or sk_test_ while you try it
TS_STRIPE_PUBLISHABLE_KEY=pk_live_...
TS_STRIPE_WEBHOOK_SECRET=whsec_...      # from the endpoint below
```

In Stripe, go to Developers → Webhooks → Add destination, pick events from **Your account**, set the API version to `2026-09-30.endive`, and choose `payment_intent.succeeded`, `refund.created`, `refund.updated`, `refund.failed` and `charge.refunded`. The URL is `https://<your API host>/v1/webhooks/stripe`, and the signing secret it gives you is your `TS_STRIPE_WEBHOOK_SECRET`. Keep the platform-fee variables (`TS_PLATFORM_FEE_BP`, `TS_PLATFORM_FEE_CENTS`) unset; the server won't start with a fee in this mode.
