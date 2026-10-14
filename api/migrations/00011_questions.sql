-- +goose Up
CREATE TABLE questions (
  id           uuid PRIMARY KEY DEFAULT uuidv7(),
  organizer_id uuid NOT NULL,
  event_id     uuid NOT NULL,
  label        text NOT NULL CHECK (char_length(label) BETWEEN 1 AND 200),
  kind         text NOT NULL CHECK (kind IN ('text', 'select', 'checkbox')),
  -- Choices for a select; the Go rules keep every other kind empty.
  options      jsonb NOT NULL DEFAULT '[]' CHECK (jsonb_typeof(options) = 'array'),
  required     boolean NOT NULL DEFAULT false,
  -- order: asked once per order. ticket: asked for every ticket.
  scope        text NOT NULL CHECK (scope IN ('order', 'ticket')),
  -- true: every ticket type. false: only those in question_applies_to, which
  -- can run empty if they are deleted; then the question is never asked.
  applies_to_all boolean NOT NULL DEFAULT true,
  position     int NOT NULL,
  created_at   timestamptz NOT NULL DEFAULT now(),
  updated_at   timestamptz NOT NULL DEFAULT now(),
  FOREIGN KEY (event_id, organizer_id) REFERENCES events (id, organizer_id) ON DELETE CASCADE,
  UNIQUE (id, organizer_id),
  UNIQUE (id, event_id)
);
CREATE TABLE question_applies_to (
  organizer_id   uuid NOT NULL,
  event_id       uuid NOT NULL,
  question_id    uuid NOT NULL,
  ticket_type_id uuid NOT NULL,
  PRIMARY KEY (question_id, ticket_type_id),
  FOREIGN KEY (question_id, organizer_id) REFERENCES questions (id, organizer_id) ON DELETE CASCADE,
  FOREIGN KEY (ticket_type_id, organizer_id) REFERENCES ticket_types (id, organizer_id) ON DELETE CASCADE,
  -- A link joins a question and a ticket type of the same event.
  FOREIGN KEY (question_id, event_id) REFERENCES questions (id, event_id) ON DELETE CASCADE,
  FOREIGN KEY (ticket_type_id, event_id) REFERENCES ticket_types (id, event_id) ON DELETE CASCADE
);
CREATE INDEX ON question_applies_to (ticket_type_id);
CREATE INDEX ON questions (event_id, position);
ALTER TABLE questions ENABLE ROW LEVEL SECURITY;
ALTER TABLE question_applies_to ENABLE ROW LEVEL SECURITY;
CREATE POLICY questions_scope ON questions
  USING (organizer_id = (SELECT app_organizer())) WITH CHECK (organizer_id = (SELECT app_organizer()));
CREATE POLICY question_applies_to_scope ON question_applies_to
  USING (organizer_id = (SELECT app_organizer())) WITH CHECK (organizer_id = (SELECT app_organizer()));

-- +goose Down
DROP TABLE question_applies_to, questions;
