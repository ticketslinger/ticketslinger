-- +goose Up
-- The buyer's "Email me my tickets again": queue_order_email (00035) for the
-- order whose code and link token match, and NULL for anything else (an
-- unknown code, a wrong token, an order without a link yet), in one
-- statement, as order_by_link does. Only ts_public can run it, and that's
-- all ts_public can write: the storefront switches to it for this one call.
-- +goose StatementBegin
CREATE FUNCTION queue_order_email_by_link(p_code text, p_token_hash bytea) RETURNS text
LANGUAGE sql VOLATILE SECURITY DEFINER SET search_path = pg_catalog, public, pg_temp AS $$
  SELECT public.queue_order_email(o.id) FROM public.orders o
   WHERE o.code = pg_catalog.upper(p_code) AND o.magic_token_hash = p_token_hash
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION queue_order_email_by_link(text, bytea) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION queue_order_email_by_link(text, bytea) TO ts_public;

-- +goose Down
DROP FUNCTION queue_order_email_by_link(text, bytea);
