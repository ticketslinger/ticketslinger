-- +goose Up
-- What the buyer paid for each ticket, before a passed-on fee: the amount a
-- refund of that ticket gives back. Prices can't change once a ticket type
-- has sales, so the backfill from today's catalogue is exact.
ALTER TABLE tickets ADD COLUMN charge_cents bigint CHECK (charge_cents >= 0);
UPDATE tickets t SET charge_cents = t.price_cents - t.discount_cents
    + CASE WHEN coalesce(tt.prices_include_tax, e.prices_include_tax) THEN 0 ELSE t.tax_cents END
  FROM ticket_types tt, events e WHERE tt.id = t.ticket_type_id AND e.id = t.event_id;
ALTER TABLE tickets ALTER COLUMN charge_cents SET NOT NULL;

-- +goose Down
ALTER TABLE tickets DROP COLUMN charge_cents;
