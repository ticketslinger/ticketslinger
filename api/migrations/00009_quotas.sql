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
  UNIQUE (id, organizer_id),
  UNIQUE (id, event_id)
);
CREATE INDEX ON quotas (event_id);
ALTER TABLE ticket_types ADD CONSTRAINT ticket_types_id_event_key UNIQUE (id, event_id);
CREATE TABLE quota_ticket_types (
  organizer_id   uuid NOT NULL,
  event_id       uuid NOT NULL,
  quota_id       uuid NOT NULL,
  ticket_type_id uuid NOT NULL,
  PRIMARY KEY (quota_id, ticket_type_id),
  FOREIGN KEY (quota_id, organizer_id) REFERENCES quotas (id, organizer_id) ON DELETE CASCADE,
  FOREIGN KEY (ticket_type_id, organizer_id) REFERENCES ticket_types (id, organizer_id) ON DELETE CASCADE,
  -- A link joins a quota and a ticket type of the same event; neither can change event while linked.
  FOREIGN KEY (quota_id, event_id) REFERENCES quotas (id, event_id) ON DELETE CASCADE,
  FOREIGN KEY (ticket_type_id, event_id) REFERENCES ticket_types (id, event_id) ON DELETE CASCADE
);
CREATE INDEX ON quota_ticket_types (ticket_type_id);
ALTER TABLE quotas ENABLE ROW LEVEL SECURITY;
ALTER TABLE quota_ticket_types ENABLE ROW LEVEL SECURITY;
CREATE POLICY quotas_scope ON quotas
  USING (organizer_id = (SELECT app_organizer())) WITH CHECK (organizer_id = (SELECT app_organizer()));
CREATE POLICY qtt_scope ON quota_ticket_types
  USING (organizer_id = (SELECT app_organizer())) WITH CHECK (organizer_id = (SELECT app_organizer()));

-- +goose Down
DROP TABLE quota_ticket_types, quotas;
ALTER TABLE ticket_types DROP CONSTRAINT ticket_types_id_event_key;
