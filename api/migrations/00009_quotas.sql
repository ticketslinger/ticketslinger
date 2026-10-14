-- +goose Up
CREATE TABLE quotas (
  id           uuid PRIMARY KEY DEFAULT uuidv7(),
  organizer_id uuid NOT NULL,
  event_id     uuid NOT NULL,
  name         text NOT NULL CHECK (length(name) BETWEEN 1 AND 120),
  size         int NOT NULL CHECK (size BETWEEN 0 AND 1000000),
  created_at   timestamptz NOT NULL DEFAULT now(),
  updated_at   timestamptz NOT NULL DEFAULT now(),
  FOREIGN KEY (event_id, organizer_id) REFERENCES events (id, organizer_id) ON DELETE CASCADE,
  UNIQUE (id, organizer_id)
);
CREATE INDEX ON quotas (event_id);
CREATE TABLE quota_ticket_types (
  organizer_id   uuid NOT NULL,
  quota_id       uuid NOT NULL,
  ticket_type_id uuid NOT NULL,
  PRIMARY KEY (quota_id, ticket_type_id),
  FOREIGN KEY (quota_id, organizer_id) REFERENCES quotas (id, organizer_id) ON DELETE CASCADE,
  FOREIGN KEY (ticket_type_id, organizer_id) REFERENCES ticket_types (id, organizer_id) ON DELETE CASCADE
);
CREATE INDEX ON quota_ticket_types (ticket_type_id);
ALTER TABLE quotas ENABLE ROW LEVEL SECURITY;
ALTER TABLE quota_ticket_types ENABLE ROW LEVEL SECURITY;
CREATE POLICY quotas_scope ON quotas
  USING (organizer_id = (SELECT app_organizer())) WITH CHECK (organizer_id = (SELECT app_organizer()));
CREATE POLICY qtt_scope ON quota_ticket_types
  USING (organizer_id = (SELECT app_organizer())) WITH CHECK (organizer_id = (SELECT app_organizer()));

-- A quota links only ticket types of its own event. Go checks this first; the
-- trigger is the backstop. Not SECURITY DEFINER: it reads under the caller's RLS.
-- +goose StatementBegin
CREATE FUNCTION qtt_same_event() RETURNS trigger LANGUAGE plpgsql SET search_path = pg_catalog, public, pg_temp AS $$
BEGIN
  IF (SELECT event_id FROM public.quotas WHERE id = NEW.quota_id)
     IS DISTINCT FROM (SELECT event_id FROM public.ticket_types WHERE id = NEW.ticket_type_id) THEN
    RAISE EXCEPTION 'quota and ticket type belong to different events' USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END $$;
-- +goose StatementEnd
CREATE TRIGGER qtt_same_event BEFORE INSERT OR UPDATE ON quota_ticket_types
  FOR EACH ROW EXECUTE FUNCTION qtt_same_event();

-- +goose Down
DROP TABLE quota_ticket_types, quotas;
DROP FUNCTION qtt_same_event();
