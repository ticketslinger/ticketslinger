-- +goose Up
ALTER TABLE organizers
  ADD COLUMN stripe_account_id text UNIQUE CHECK (stripe_account_id ~ '^acct_[A-Za-z0-9]+$'),
  ADD COLUMN stripe_charges_enabled boolean NOT NULL DEFAULT false,
  ADD COLUMN stripe_details_submitted boolean NOT NULL DEFAULT false;
-- Stripe can't charge a card less than $0.50.
ALTER TABLE ticket_types ADD CONSTRAINT ticket_types_chargeable CHECK (price_cents = 0 OR price_cents >= 50);

-- account.updated webhooks keep the flags current. ts_system already reads
-- the whole row (00016).
GRANT UPDATE (stripe_charges_enabled, stripe_details_submitted) ON organizers TO ts_system;
CREATE POLICY organizers_system_update ON organizers FOR UPDATE TO ts_system
  USING (id = (SELECT app_scope())) WITH CHECK (id = (SELECT app_scope()));

-- Which organizer a Stripe event belongs to, before any scope exists.
CREATE FUNCTION organizer_for_stripe_account(p_account text) RETURNS uuid LANGUAGE sql STABLE SECURITY DEFINER
  SET search_path = pg_catalog, public, pg_temp AS
  $$ SELECT id FROM public.organizers WHERE stripe_account_id = p_account $$;
REVOKE ALL ON FUNCTION organizer_for_stripe_account(text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION organizer_for_stripe_account(text) TO ts_app;

-- +goose Down
DROP FUNCTION organizer_for_stripe_account(text);
DROP POLICY organizers_system_update ON organizers;
REVOKE UPDATE (stripe_charges_enabled, stripe_details_submitted) ON organizers FROM ts_system;
ALTER TABLE ticket_types DROP CONSTRAINT ticket_types_chargeable;
ALTER TABLE organizers DROP COLUMN stripe_details_submitted, DROP COLUMN stripe_charges_enabled, DROP COLUMN stripe_account_id;
