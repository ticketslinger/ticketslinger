-- +goose Up
-- slug is citext, so `~` matched case-insensitively and let "Acme" through.
ALTER TABLE organizers DROP CONSTRAINT organizers_slug_check;
ALTER TABLE organizers ADD CONSTRAINT organizers_slug_check
  CHECK (slug::text ~ '^[a-z0-9](-?[a-z0-9])*$' AND length(slug::text) BETWEEN 3 AND 40);
ALTER TABLE organizers ADD CONSTRAINT organizers_name_check CHECK (length(name) BETWEEN 1 AND 120);

-- +goose Down
ALTER TABLE organizers DROP CONSTRAINT organizers_name_check;
ALTER TABLE organizers DROP CONSTRAINT organizers_slug_check;
ALTER TABLE organizers ADD CONSTRAINT organizers_slug_check
  CHECK (slug ~ '^[a-z0-9](-?[a-z0-9])*$' AND length(slug) BETWEEN 3 AND 40);
