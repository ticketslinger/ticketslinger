# ticketslinger
The worlds greatest open-source ticket selling platform.

## Run it locally

```sh
make dev-db   # PostgreSQL 18 on localhost:54329, roles ts_owner and ts_app
make test     # Go tests (with -race), then bun test
```

`make dev-db-down` throws the database away.
