-- +goose Up
-- Ticket types have carried a position since 00008, but nothing numbered the
-- old rows. Number them 1..n per event, oldest first, then hold the line.
UPDATE ticket_types t SET position = n.rn
  FROM (SELECT id, row_number() OVER (PARTITION BY event_id ORDER BY created_at, id) AS rn FROM ticket_types) n
 WHERE t.id = n.id;
ALTER TABLE ticket_types ALTER COLUMN position DROP DEFAULT;
ALTER TABLE ticket_types ADD CONSTRAINT ticket_types_position_positive CHECK (position >= 1);

-- +goose Down
ALTER TABLE ticket_types DROP CONSTRAINT ticket_types_position_positive;
ALTER TABLE ticket_types ALTER COLUMN position SET DEFAULT 0;
