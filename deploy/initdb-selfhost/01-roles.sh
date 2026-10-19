#!/bin/sh
# Self-hosting twin of ../initdb/01-roles.sql (which is dev/CI only, with fixed
# passwords). Same roles, but the two login passwords come from the environment.
# Runs once, on the first start of an empty data volume.
set -eu
: "${TS_OWNER_PASSWORD:?set TS_OWNER_PASSWORD in deploy/.env}"
: "${TS_APP_PASSWORD:?set TS_APP_PASSWORD in deploy/.env}"

psql -v ON_ERROR_STOP=1 -U "$POSTGRES_USER" -d "$POSTGRES_DB" \
  -v owner_pw="$TS_OWNER_PASSWORD" -v app_pw="$TS_APP_PASSWORD" <<'SQL'
CREATE ROLE ts_owner LOGIN PASSWORD :'owner_pw';
CREATE ROLE ts_app LOGIN PASSWORD :'app_pw' NOBYPASSRLS;
ALTER DATABASE ticketslinger OWNER TO ts_owner;
REVOKE TEMPORARY ON DATABASE ticketslinger FROM PUBLIC;
CREATE ROLE ts_public NOLOGIN NOBYPASSRLS;
CREATE ROLE ts_system NOLOGIN NOBYPASSRLS;
GRANT ts_public TO ts_app WITH INHERIT FALSE, SET TRUE;
GRANT ts_system TO ts_app WITH INHERIT FALSE, SET TRUE;
\connect ticketslinger
ALTER SCHEMA public OWNER TO ts_owner;
CREATE EXTENSION IF NOT EXISTS citext;
SQL
