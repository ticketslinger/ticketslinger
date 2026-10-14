-- +goose Up
CREATE TABLE idempotency_keys (
  scope        text NOT NULL,
  key          text NOT NULL CHECK (length(key) BETWEEN 1 AND 255),
  request_hash bytea NOT NULL,
  owner        uuid NOT NULL,    -- the run that reserved the key; only it may store or release
  status       int,              -- NULL while the first request is running
  content_type text,
  body         bytea,
  expires_at   timestamptz NOT NULL DEFAULT now() + interval '24 hours',
  PRIMARY KEY (scope, key)
);
CREATE INDEX ON idempotency_keys (expires_at);

-- +goose Down
DROP TABLE idempotency_keys;
