-- +goose Up
-- When the pending (unconfirmed) TOTP secret was issued. A secret left
-- unconfirmed for long can't be confirmed later, after the account changed.
ALTER TABLE users ADD COLUMN totp_pending_at timestamptz;

-- +goose Down
ALTER TABLE users DROP COLUMN totp_pending_at;
