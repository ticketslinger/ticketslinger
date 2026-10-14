-- +goose Up
CREATE TABLE promo_codes (
  id           uuid PRIMARY KEY DEFAULT uuidv7(),
  organizer_id uuid NOT NULL,
  event_id     uuid NOT NULL,
  code         text NOT NULL CHECK (code ~ '^[A-Z0-9-]{3,32}$'),
  kind         text NOT NULL CHECK (kind IN ('percent', 'amount')),
  -- percent: basis points (1 to 10000); amount: cents (up to $100,000).
  value        bigint NOT NULL CHECK ((kind = 'percent' AND value BETWEEN 1 AND 10000) OR (kind = 'amount' AND value BETWEEN 1 AND 10000000)),
  max_uses     int CHECK (max_uses > 0),
  used_count   int NOT NULL DEFAULT 0 CHECK (used_count >= 0),
  valid_from   timestamptz,
  valid_until  timestamptz,
  created_at   timestamptz NOT NULL DEFAULT now(),
  updated_at   timestamptz NOT NULL DEFAULT now(),
  CHECK (valid_from IS NULL OR valid_until IS NULL OR valid_until > valid_from),
  FOREIGN KEY (event_id, organizer_id) REFERENCES events (id, organizer_id) ON DELETE CASCADE,
  UNIQUE (event_id, code),
  UNIQUE (id, organizer_id),
  UNIQUE (id, event_id)
);
-- Which ticket types a code discounts (no rows: all of them).
CREATE TABLE promo_applies_to (
  organizer_id   uuid NOT NULL,
  event_id       uuid NOT NULL,
  promo_code_id  uuid NOT NULL,
  ticket_type_id uuid NOT NULL,
  PRIMARY KEY (promo_code_id, ticket_type_id),
  FOREIGN KEY (promo_code_id, organizer_id) REFERENCES promo_codes (id, organizer_id) ON DELETE CASCADE,
  FOREIGN KEY (ticket_type_id, organizer_id) REFERENCES ticket_types (id, organizer_id) ON DELETE CASCADE,
  -- A link joins a code and a ticket type of the same event.
  FOREIGN KEY (promo_code_id, event_id) REFERENCES promo_codes (id, event_id) ON DELETE CASCADE,
  FOREIGN KEY (ticket_type_id, event_id) REFERENCES ticket_types (id, event_id) ON DELETE CASCADE
);
-- Which hidden ticket types a code reveals.
CREATE TABLE promo_unlocks (LIKE promo_applies_to INCLUDING ALL);
ALTER TABLE promo_unlocks
  ADD FOREIGN KEY (promo_code_id, organizer_id) REFERENCES promo_codes (id, organizer_id) ON DELETE CASCADE,
  ADD FOREIGN KEY (ticket_type_id, organizer_id) REFERENCES ticket_types (id, organizer_id) ON DELETE CASCADE,
  ADD FOREIGN KEY (promo_code_id, event_id) REFERENCES promo_codes (id, event_id) ON DELETE CASCADE,
  ADD FOREIGN KEY (ticket_type_id, event_id) REFERENCES ticket_types (id, event_id) ON DELETE CASCADE;
CREATE INDEX ON promo_applies_to (ticket_type_id);
CREATE INDEX ON promo_unlocks (ticket_type_id);
ALTER TABLE promo_codes ENABLE ROW LEVEL SECURITY;
ALTER TABLE promo_applies_to ENABLE ROW LEVEL SECURITY;
ALTER TABLE promo_unlocks ENABLE ROW LEVEL SECURITY;
CREATE POLICY promo_codes_scope ON promo_codes
  USING (organizer_id = (SELECT app_organizer())) WITH CHECK (organizer_id = (SELECT app_organizer()));
CREATE POLICY promo_applies_to_scope ON promo_applies_to
  USING (organizer_id = (SELECT app_organizer())) WITH CHECK (organizer_id = (SELECT app_organizer()));
CREATE POLICY promo_unlocks_scope ON promo_unlocks
  USING (organizer_id = (SELECT app_organizer())) WITH CHECK (organizer_id = (SELECT app_organizer()));

-- +goose Down
DROP TABLE promo_unlocks, promo_applies_to, promo_codes;
