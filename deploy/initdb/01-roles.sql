-- Local development and CI only. Production roles are created by Ansible.
CREATE ROLE ts_owner LOGIN PASSWORD 'ts_owner';
CREATE ROLE ts_app LOGIN PASSWORD 'ts_app' NOBYPASSRLS;
ALTER DATABASE ticketslinger OWNER TO ts_owner;
REVOKE TEMPORARY ON DATABASE ticketslinger FROM PUBLIC;
-- Roles the API switches to inside a transaction. NOLOGIN: only ts_app can
-- become them, with SET ROLE, and without inheriting their rights or policies.
CREATE ROLE ts_public NOLOGIN NOBYPASSRLS;
CREATE ROLE ts_system NOLOGIN NOBYPASSRLS;
GRANT ts_public TO ts_app WITH INHERIT FALSE, SET TRUE;
GRANT ts_system TO ts_app WITH INHERIT FALSE, SET TRUE;
\connect ticketslinger
ALTER SCHEMA public OWNER TO ts_owner;
CREATE EXTENSION IF NOT EXISTS citext;
ALTER ROLE ts_owner CREATEDB;
