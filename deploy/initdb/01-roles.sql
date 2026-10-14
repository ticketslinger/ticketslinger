-- Local development and CI only. Production roles are created by Ansible.
CREATE ROLE ts_owner LOGIN PASSWORD 'ts_owner';
CREATE ROLE ts_app LOGIN PASSWORD 'ts_app' NOBYPASSRLS;
ALTER DATABASE ticketslinger OWNER TO ts_owner;
REVOKE TEMPORARY ON DATABASE ticketslinger FROM PUBLIC;
\connect ticketslinger
ALTER SCHEMA public OWNER TO ts_owner;
CREATE EXTENSION IF NOT EXISTS citext;
ALTER ROLE ts_owner CREATEDB;
