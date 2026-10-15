-- +goose Up
-- The organizer a payment belongs to, from Stripe's own ids. A payment stays
-- with the organizer that took it even after its Stripe account moves to
-- another organizer, so a late event about it still finds its order.
-- Matching the account too means a signed event from another account can't
-- name our intent and land anywhere.
CREATE FUNCTION organizer_for_payment(p_intent text, p_account text) RETURNS uuid LANGUAGE sql STABLE SECURITY DEFINER
  SET search_path = pg_catalog, public, pg_temp AS
  $$ SELECT organizer_id FROM public.payments WHERE payment_intent_id = p_intent AND stripe_account_id = p_account $$;
REVOKE ALL ON FUNCTION organizer_for_payment(text, text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION organizer_for_payment(text, text) TO ts_app;

-- +goose Down
DROP FUNCTION organizer_for_payment(text, text);
