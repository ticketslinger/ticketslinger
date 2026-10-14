-- +goose Up
CREATE TABLE users (
  id                uuid PRIMARY KEY DEFAULT uuidv7(),
  email             citext NOT NULL UNIQUE,
  name              text NOT NULL,
  password_hash     text,
  email_verified_at timestamptz,
  totp_secret       bytea,
  totp_enabled_at   timestamptz,
  totp_last_step    bigint,
  failed_logins     int NOT NULL DEFAULT 0,
  locked_until      timestamptz,
  created_at        timestamptz NOT NULL DEFAULT now(),
  updated_at        timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE sessions (
  id           uuid PRIMARY KEY DEFAULT uuidv7(),
  user_id      uuid NOT NULL REFERENCES users ON DELETE CASCADE,
  token_hash   bytea NOT NULL UNIQUE,
  mfa          boolean NOT NULL,
  ip           inet,
  user_agent   text,
  expires_at   timestamptz NOT NULL,
  created_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX ON sessions (user_id);

CREATE TABLE passkeys (
  id            uuid PRIMARY KEY DEFAULT uuidv7(),
  user_id       uuid NOT NULL REFERENCES users ON DELETE CASCADE,
  credential_id bytea NOT NULL UNIQUE,
  credential    jsonb NOT NULL,
  name          text NOT NULL,
  last_used_at  timestamptz,
  created_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX ON passkeys (user_id);

CREATE TABLE webauthn_challenges (
  id         uuid PRIMARY KEY DEFAULT uuidv7(),
  user_id    uuid REFERENCES users ON DELETE CASCADE,
  kind       text NOT NULL CHECK (kind IN ('register', 'login')),
  data       jsonb NOT NULL,
  expires_at timestamptz NOT NULL
);

CREATE TABLE email_tokens (
  token_hash bytea PRIMARY KEY,
  user_id    uuid NOT NULL REFERENCES users ON DELETE CASCADE,
  purpose    text NOT NULL CHECK (purpose IN ('verify')),
  expires_at timestamptz NOT NULL
);

CREATE TABLE organizers (
  id         uuid PRIMARY KEY DEFAULT uuidv7(),
  name       text NOT NULL,
  slug       citext NOT NULL UNIQUE CHECK (slug ~ '^[a-z0-9](-?[a-z0-9])*$' AND length(slug) BETWEEN 3 AND 40),
  created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE memberships (
  organizer_id uuid NOT NULL REFERENCES organizers ON DELETE CASCADE,
  user_id      uuid NOT NULL REFERENCES users ON DELETE CASCADE,
  role         text NOT NULL CHECK (role IN ('owner', 'admin', 'checkin')),
  created_at   timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (organizer_id, user_id)
);
CREATE INDEX ON memberships (user_id);

CREATE TABLE audit_log (
  id            uuid PRIMARY KEY DEFAULT uuidv7(),
  organizer_id  uuid NOT NULL REFERENCES organizers ON DELETE CASCADE,
  actor_user_id uuid REFERENCES users ON DELETE SET NULL,
  action        text NOT NULL,
  entity_type   text NOT NULL,
  entity_id     uuid NOT NULL,
  before        jsonb,
  after         jsonb,
  at            timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX ON audit_log (organizer_id, at DESC);

CREATE FUNCTION app_user() RETURNS uuid LANGUAGE sql STABLE AS
  $$ SELECT NULLIF(current_setting('app.user_id', true), '')::uuid $$;
CREATE FUNCTION app_organizer() RETURNS uuid LANGUAGE sql STABLE AS
  $$ SELECT NULLIF(current_setting('app.organizer_id', true), '')::uuid $$;

ALTER TABLE organizers ENABLE ROW LEVEL SECURITY;
ALTER TABLE memberships ENABLE ROW LEVEL SECURITY;
ALTER TABLE audit_log ENABLE ROW LEVEL SECURITY;

CREATE POLICY member_reads ON memberships FOR SELECT
  USING (user_id = app_user() OR organizer_id = app_organizer());
CREATE POLICY org_reads ON organizers FOR SELECT
  USING (id = app_organizer() OR id IN (SELECT organizer_id FROM memberships WHERE user_id = app_user()));
CREATE POLICY org_updates ON organizers FOR UPDATE
  USING (id = app_organizer()) WITH CHECK (id = app_organizer());
CREATE POLICY audit_scope ON audit_log
  USING (organizer_id = app_organizer()) WITH CHECK (organizer_id = app_organizer());

-- Creating an organizer is the one write that has no tenant yet. It runs as
-- the schema owner, makes the caller the only owner, and nothing else.
-- +goose StatementBegin
CREATE FUNCTION create_organizer(p_user uuid, p_name text, p_slug citext) RETURNS uuid
LANGUAGE plpgsql SECURITY DEFINER SET search_path = public AS $$
DECLARE org uuid;
BEGIN
  INSERT INTO organizers(name, slug) VALUES (p_name, p_slug) RETURNING id INTO org;
  INSERT INTO memberships(organizer_id, user_id, role) VALUES (org, p_user, 'owner');
  RETURN org;
END $$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION create_organizer(uuid, text, citext) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION create_organizer(uuid, text, citext) TO ts_app;

-- +goose Down
DROP FUNCTION create_organizer(uuid, text, citext);
DROP TABLE audit_log, memberships, organizers, email_tokens, webauthn_challenges, passkeys, sessions, users;
DROP FUNCTION app_organizer(), app_user();
