-- +goose Up
-- The storefront's own paths share the first URL segment with organizer slugs.
-- Say which organizers are in the way before the CHECK would refuse them.
-- +goose StatementBegin
DO $$
DECLARE taken text;
BEGIN
  SELECT string_agg(slug::text, ', ' ORDER BY slug) INTO taken FROM organizers
  WHERE slug::text IN ('checkout', 'orders', 'assets', 'api', 'healthz');
  IF taken IS NOT NULL THEN
    RAISE EXCEPTION 'organizers use slugs the storefront needs: %. Rename them, then migrate again.', taken;
  END IF;
END $$;
-- +goose StatementEnd
ALTER TABLE organizers ADD CONSTRAINT organizers_slug_not_reserved
  CHECK (slug::text NOT IN ('checkout', 'orders', 'assets', 'api', 'healthz'));

-- +goose Down
ALTER TABLE organizers DROP CONSTRAINT organizers_slug_not_reserved;
