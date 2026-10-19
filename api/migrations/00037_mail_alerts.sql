-- +goose Up
-- One row per quota warning already sent (key = threshold + period), so a
-- warning goes out once. ts_app may add rows and read them, never change or
-- remove one. Not tenant data: no organizer_id, no RLS.
CREATE TABLE mail_alerts (
  key        text PRIMARY KEY CHECK (length(key) <= 60),
  created_at timestamptz NOT NULL DEFAULT now()
);
REVOKE UPDATE, DELETE ON mail_alerts FROM ts_app;

-- +goose Down
DROP TABLE mail_alerts;
