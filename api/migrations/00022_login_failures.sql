-- +goose Up
-- Failed sign-ins per account and address, so a stranger can't lock the
-- owner out from somewhere else. users.failed_logins and users.locked_until
-- are no longer read or written; a later release drops them.
CREATE TABLE login_failures (
  user_id      uuid NOT NULL REFERENCES users ON DELETE CASCADE,
  ip           text NOT NULL,           -- '' when the address is unknown; an IPv6 /64 is one address
  failures     int NOT NULL DEFAULT 0,
  locked_until timestamptz,
  updated_at   timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (user_id, ip)
);
CREATE INDEX ON login_failures (updated_at);

-- +goose Down
DROP TABLE login_failures;
