-- +goose Up
CREATE TABLE events (
  id                 uuid PRIMARY KEY DEFAULT uuidv7(),
  organizer_id       uuid NOT NULL REFERENCES organizers ON DELETE CASCADE,
  slug               text NOT NULL CHECK (slug ~ '^[a-z0-9](-?[a-z0-9])*$' AND length(slug) BETWEEN 3 AND 60),
  name               text NOT NULL CHECK (length(name) BETWEEN 1 AND 160),
  description        text NOT NULL DEFAULT '' CHECK (length(description) <= 20000),
  venue_name         text NOT NULL DEFAULT '' CHECK (length(venue_name) <= 160),
  venue_address      text NOT NULL DEFAULT '' CHECK (length(venue_address) <= 500),
  timezone           text NOT NULL,
  starts_at          timestamptz NOT NULL,
  ends_at            timestamptz NOT NULL CHECK (ends_at > starts_at),
  status             text NOT NULL DEFAULT 'draft' CHECK (status IN ('draft', 'live', 'ended', 'cancelled')),
  fee_mode           text NOT NULL DEFAULT 'pass' CHECK (fee_mode IN ('absorb', 'pass')),
  currency           char(3) NOT NULL DEFAULT 'USD' CHECK (currency = 'USD'),
  tax_rate_bp        int NOT NULL DEFAULT 0 CHECK (tax_rate_bp BETWEEN 0 AND 10000),
  prices_include_tax boolean NOT NULL DEFAULT false,
  created_at         timestamptz NOT NULL DEFAULT now(),
  updated_at         timestamptz NOT NULL DEFAULT now(),
  UNIQUE (organizer_id, slug),
  UNIQUE (id, organizer_id)               -- target for children's composite FK
);
CREATE INDEX ON events (organizer_id, starts_at);
ALTER TABLE events ENABLE ROW LEVEL SECURITY;
CREATE POLICY events_scope ON events
  USING (organizer_id = (SELECT app_organizer())) WITH CHECK (organizer_id = (SELECT app_organizer()));

-- +goose Down
DROP TABLE events;
