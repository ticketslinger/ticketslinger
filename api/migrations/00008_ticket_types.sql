-- +goose Up
CREATE TABLE ticket_types (
  id                 uuid PRIMARY KEY DEFAULT uuidv7(),
  organizer_id       uuid NOT NULL,
  event_id           uuid NOT NULL,
  name               text NOT NULL CHECK (length(name) BETWEEN 1 AND 120),
  description        text NOT NULL DEFAULT '' CHECK (length(description) <= 2000),
  price_cents        bigint NOT NULL CHECK (price_cents BETWEEN 0 AND 10000000),
  tax_rate_bp        int CHECK (tax_rate_bp BETWEEN 0 AND 10000),
  prices_include_tax boolean,
  sales_start        timestamptz,
  sales_end          timestamptz CHECK (sales_end IS NULL OR sales_start IS NULL OR sales_end > sales_start),
  min_per_order      int NOT NULL DEFAULT 1 CHECK (min_per_order BETWEEN 1 AND 100),
  max_per_order      int NOT NULL DEFAULT 10 CHECK (max_per_order BETWEEN 1 AND 100),
  hidden             boolean NOT NULL DEFAULT false,
  position           int NOT NULL DEFAULT 0,
  created_at         timestamptz NOT NULL DEFAULT now(),
  updated_at         timestamptz NOT NULL DEFAULT now(),
  CHECK (min_per_order <= max_per_order),
  FOREIGN KEY (event_id, organizer_id) REFERENCES events (id, organizer_id) ON DELETE CASCADE,
  UNIQUE (id, organizer_id)
);
CREATE INDEX ON ticket_types (event_id, position);
ALTER TABLE ticket_types ENABLE ROW LEVEL SECURITY;
CREATE POLICY ticket_types_scope ON ticket_types
  USING (organizer_id = (SELECT app_organizer())) WITH CHECK (organizer_id = (SELECT app_organizer()));

-- +goose Down
DROP TABLE ticket_types;
