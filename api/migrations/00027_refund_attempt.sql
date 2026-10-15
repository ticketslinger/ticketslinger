-- +goose Up
-- Which idempotency key the order's refund uses: refund-<order>-<attempt>.
-- Stripe replays a refusal under one key for 24 hours, longer than the job's
-- retries, so a definite refusal moves the order to its next key. Nothing
-- short of a refusal does: a refund that may have happened keeps its key.
ALTER TABLE orders ADD COLUMN refund_attempt int NOT NULL DEFAULT 0 CHECK (refund_attempt >= 0);

-- +goose Down
ALTER TABLE orders DROP COLUMN refund_attempt;
