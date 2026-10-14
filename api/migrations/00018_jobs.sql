-- +goose Up
CREATE TABLE jobs (
  id           uuid PRIMARY KEY DEFAULT uuidv7(),
  kind         text NOT NULL CHECK (kind ~ '^[a-z_]{1,40}$'),
  payload      jsonb NOT NULL,
  run_at       timestamptz NOT NULL DEFAULT now(),
  attempts     int NOT NULL DEFAULT 0,
  last_error   text,
  locked_until timestamptz,
  created_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX ON jobs (run_at);
-- Webhook and checkout transactions enqueue as ts_system.
GRANT INSERT ON jobs TO ts_system;

-- +goose Down
DROP TABLE jobs;
