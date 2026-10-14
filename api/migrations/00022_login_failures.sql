-- +goose Up
-- Sign-in attempts that didn't succeed, per email and address, so a stranger
-- can't lock the owner out from somewhere else. account is an HMAC-SHA256 of
-- the normalised email under a key derived from TS_SECRET_KEY: emails with
-- no account are counted the same way, and the table names nobody.
-- users.failed_logins and users.locked_until are no longer read or written;
-- a later release drops them.
CREATE TABLE login_failures (
  account      bytea NOT NULL CHECK (length(account) = 32),
  ip           text NOT NULL,           -- '' when the address is unknown; an IPv6 /64 is one address
  failures     int NOT NULL DEFAULT 0,
  locked_until timestamptz,
  updated_at   timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (account, ip)
);
CREATE INDEX ON login_failures (updated_at);

-- +goose Down
DROP TABLE login_failures;
