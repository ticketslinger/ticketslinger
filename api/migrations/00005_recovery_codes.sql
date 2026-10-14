-- +goose Up
-- Identity data like passwords: no RLS, ts_app only (default privileges).
CREATE TABLE recovery_codes (
  id         uuid PRIMARY KEY DEFAULT uuidv7(),
  user_id    uuid NOT NULL REFERENCES users ON DELETE CASCADE,
  code_hash  text NOT NULL,
  used_at    timestamptz,
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX ON recovery_codes (user_id) WHERE used_at IS NULL;

-- +goose Down
DROP TABLE recovery_codes;
