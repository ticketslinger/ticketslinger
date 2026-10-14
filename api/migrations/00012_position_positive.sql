-- +goose Up
ALTER TABLE questions ADD CONSTRAINT questions_position_positive CHECK (position >= 1);

-- +goose Down
ALTER TABLE questions DROP CONSTRAINT questions_position_positive;
