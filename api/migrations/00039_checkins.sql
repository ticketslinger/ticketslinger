-- +goose Up
-- One row per scan or manual check-in, whatever it decided. value_hash is the
-- sha256 of the scanned text: the text itself is the ticket's credential, so
-- it is never stored.
CREATE TABLE checkins (
  id               uuid PRIMARY KEY DEFAULT uuidv7(),
  organizer_id     uuid NOT NULL,
  event_id         uuid NOT NULL,
  ticket_id        uuid,
  value_hash       bytea CHECK (length(value_hash) = 32),
  method           text NOT NULL CHECK (method IN ('scan', 'manual')),
  gate             text CHECK (char_length(gate) <= 40),
  result           text NOT NULL CHECK (result IN ('accepted', 'duplicate', 'invalid', 'void')),
  actor_user_id    uuid REFERENCES users,
  actor_api_key_id uuid REFERENCES api_keys,
  at               timestamptz NOT NULL DEFAULT statement_timestamp(),
  FOREIGN KEY (event_id, organizer_id) REFERENCES events (id, organizer_id),
  FOREIGN KEY (ticket_id, organizer_id) REFERENCES tickets (id, organizer_id),
  CHECK (num_nonnulls(actor_user_id, actor_api_key_id) = 1),
  CHECK ((method = 'scan') = (value_hash IS NOT NULL))
);
CREATE INDEX ON checkins (event_id, at DESC);
ALTER TABLE checkins ENABLE ROW LEVEL SECURITY;
CREATE POLICY checkins_scope ON checkins USING (organizer_id = (SELECT app_organizer())) WITH CHECK (organizer_id = (SELECT app_organizer()));
-- Append-only for ts_app.
REVOKE ALL ON checkins FROM ts_app;
GRANT SELECT, INSERT ON checkins TO ts_app;

-- The check-in itself is ts_app's own conditional UPDATE of this one column.
GRANT UPDATE (checked_in_at) ON tickets TO ts_app;

-- ts_app moves tickets one way only: valid to void, pointed at a refund once,
-- released once, and only as it voids them; checked in once, from nothing to
-- the time of the check-in itself (never back- or forward-dated), and only
-- while valid. Nothing it does brings a refunded ticket back or un-checks one.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION tickets_app_one_way() RETURNS trigger LANGUAGE plpgsql
  SET search_path = pg_catalog, public, pg_temp AS $$
BEGIN
  IF current_user = 'ts_app' AND (
       (NEW.status IS DISTINCT FROM OLD.status AND NOT (OLD.status = 'valid' AND NEW.status = 'void'))
    OR (NEW.refund_id IS DISTINCT FROM OLD.refund_id AND OLD.refund_id IS NOT NULL)
    OR (NEW.released_at IS DISTINCT FROM OLD.released_at AND OLD.released_at IS NOT NULL)
    OR ((NEW.refund_id IS DISTINCT FROM OLD.refund_id OR NEW.released_at IS DISTINCT FROM OLD.released_at)
        AND NEW.status IS DISTINCT FROM 'void')
    OR (NEW.checked_in_at IS DISTINCT FROM OLD.checked_in_at
        AND (OLD.checked_in_at IS NOT NULL OR NEW.checked_in_at IS NULL
             OR NEW.checked_in_at <> statement_timestamp() OR OLD.status <> 'valid'))) THEN
    RAISE EXCEPTION 'tickets: one way only: valid to void, checked in once' USING ERRCODE = 'insufficient_privilege';
  END IF;
  RETURN NEW;
END $$;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION tickets_app_one_way() RETURNS trigger LANGUAGE plpgsql
  SET search_path = pg_catalog, public, pg_temp AS $$
BEGIN
  IF current_user = 'ts_app' AND (
       (NEW.status IS DISTINCT FROM OLD.status AND NOT (OLD.status = 'valid' AND NEW.status = 'void'))
    OR (NEW.refund_id IS DISTINCT FROM OLD.refund_id AND OLD.refund_id IS NOT NULL)
    OR (NEW.released_at IS DISTINCT FROM OLD.released_at AND OLD.released_at IS NOT NULL)
    OR ((NEW.refund_id IS DISTINCT FROM OLD.refund_id OR NEW.released_at IS DISTINCT FROM OLD.released_at)
        AND NEW.status IS DISTINCT FROM 'void')) THEN
    RAISE EXCEPTION 'tickets: only valid to void, once' USING ERRCODE = 'insufficient_privilege';
  END IF;
  RETURN NEW;
END $$;
-- +goose StatementEnd
REVOKE UPDATE (checked_in_at) ON tickets FROM ts_app;
DROP TABLE checkins;
