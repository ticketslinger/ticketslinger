-- +goose Up
-- Takedown: staff suspend an organizer, lift it, or refund one of its
-- events. A suspended organizer sells nothing and its team and keys change
-- nothing (db.WithPrincipal makes their transactions READ ONLY). Nothing is
-- deleted: the action rows and the takedown's targets stay as evidence.
-- search_path ends in pg_temp so a temp table cannot shadow a real one.

-- Every staff write, ever. Append-only for every app role.
CREATE TABLE staff_actions (
  id               uuid PRIMARY KEY DEFAULT uuidv7(),
  staff_session_id uuid NOT NULL REFERENCES staff_sessions,
  user_id          uuid NOT NULL REFERENCES staff,
  organizer_id     uuid NOT NULL REFERENCES organizers ON DELETE RESTRICT,
  kind             text NOT NULL CHECK (kind IN ('suspend', 'lift', 'refund_event')),
  event_id         uuid REFERENCES events ON DELETE RESTRICT,
  reason           text NOT NULL CHECK (length(reason) BETWEEN 10 AND 500),
  at               timestamptz NOT NULL DEFAULT now(),
  claimed_xid      xid8,             -- refund_event only: the transaction running the refunds
  finished_at      timestamptz,      -- refund_event only: set once the refunds are queued
  result           jsonb,            -- refund_event only: orders, amount_cents, fee_cents, pending_orders
  CHECK ((kind = 'refund_event') = (event_id IS NOT NULL)),
  CHECK (kind = 'refund_event' OR finished_at IS NULL)
);

-- One row while an organizer is suspended. ts_app and ts_system may only
-- read it through organizer_suspended(); nobody but the definer functions
-- below writes it.
CREATE TABLE organizer_suspensions (
  organizer_id    uuid PRIMARY KEY REFERENCES organizers ON DELETE RESTRICT,
  staff_action_id uuid NOT NULL REFERENCES staff_actions, -- the suspend action
  suspended_at    timestamptz NOT NULL DEFAULT now()
);

-- No role but the owner touches either table. Row security with no policy
-- is a second wall: a grant added by mistake still shows no row.
REVOKE ALL ON organizer_suspensions, staff_actions FROM ts_app, ts_public, ts_system;
ALTER TABLE organizer_suspensions ENABLE ROW LEVEL SECURITY;
ALTER TABLE staff_actions ENABLE ROW LEVEL SECURITY;

-- Refunds staff start for a takedown. refunds_app_insert still limits
-- ts_app to 'organizer' and 'event_cancelled'.
ALTER TABLE refunds DROP CONSTRAINT refunds_reason_check,
  ADD CONSTRAINT refunds_reason_check CHECK (reason IN ('late_payment', 'organizer', 'event_cancelled', 'platform'));

-- +goose StatementBegin
-- Whether p_org is suspended. Says nothing else.
CREATE FUNCTION organizer_suspended(p_org uuid) RETURNS boolean LANGUAGE sql STABLE SECURITY DEFINER
  SET search_path = pg_catalog, public, pg_temp AS
  $$ SELECT EXISTS (SELECT 1 FROM public.organizer_suspensions s WHERE s.organizer_id = p_org) $$;

-- One staff action on the organizer of the caller's live staff session,
-- in an ordinary read-write user transaction. Re-checks everything, locks
-- the organizer first so actions on one organizer run one after another
-- (and after any member write holding that row), and records the action in
-- staff_actions and in the organizer's own audit log. The alert email is
-- the caller's, in the same transaction.
CREATE FUNCTION staff_act(p_staff_session uuid, p_login_session uuid, p_kind text, p_event uuid, p_reason text) RETURNS uuid
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, public, pg_temp AS $$
DECLARE usr uuid := public.app_user();
        org uuid;
        aid uuid;
BEGIN
  -- Lock first, check after: a session ended while this waited doesn't act.
  SELECT ss.organizer_id INTO org FROM public.staff_sessions ss WHERE ss.id = p_staff_session AND ss.user_id = usr;
  PERFORM 1 FROM public.organizers WHERE id = org FOR UPDATE;
  IF usr IS NULL OR org IS NULL OR NOT public.app_is_staff() OR NOT EXISTS (
       SELECT 1 FROM public.staff_sessions ss
         JOIN public.sessions se ON se.id = ss.session_id AND se.user_id = ss.user_id
                                 AND se.mfa AND se.expires_at > pg_catalog.clock_timestamp()
        WHERE ss.id = p_staff_session
          AND ss.organizer_id = org
          AND ss.user_id = usr
          AND ss.session_id = p_login_session
          AND ss.ended_at IS NULL
          AND ss.expires_at > pg_catalog.clock_timestamp()) THEN
    RAISE EXCEPTION 'staff_act: not allowed' USING ERRCODE = '42501';
  END IF;
  IF p_kind = 'suspend' AND public.organizer_suspended(org) THEN
    RAISE EXCEPTION 'already_suspended' USING ERRCODE = '55000';
  END IF;
  IF p_kind IN ('lift', 'refund_event') AND NOT public.organizer_suspended(org) THEN
    RAISE EXCEPTION 'not_suspended' USING ERRCODE = '55000';
  END IF;
  IF p_kind = 'refund_event' AND NOT EXISTS (SELECT 1 FROM public.events e WHERE e.id = p_event AND e.organizer_id = org) THEN
    RAISE EXCEPTION 'staff_act: no such event' USING ERRCODE = 'no_data_found';
  END IF;
  INSERT INTO public.staff_actions(staff_session_id, user_id, organizer_id, kind, event_id, reason)
  VALUES (p_staff_session, usr, org, p_kind, p_event, p_reason)
  RETURNING id INTO aid;
  IF p_kind = 'suspend' THEN
    INSERT INTO public.organizer_suspensions(organizer_id, staff_action_id) VALUES (org, aid);
  ELSIF p_kind = 'lift' THEN
    DELETE FROM public.organizer_suspensions WHERE organizer_id = org;
  END IF;
  INSERT INTO public.audit_log(organizer_id, actor_user_id, actor_staff_session_id, action, entity_type, entity_id, after)
  VALUES (org, usr, p_staff_session,
          CASE p_kind WHEN 'suspend' THEN 'staff.suspended' WHEN 'lift' THEN 'staff.lifted' ELSE 'staff.refund_requested' END,
          CASE WHEN p_kind = 'refund_event' THEN 'event' ELSE 'organizer' END, coalesce(p_event, org),
          pg_catalog.jsonb_build_object('reason', p_reason));
  RETURN aid;
END $$;

-- The system role's first step in a staff refund: locks the action, so a
-- second submit waits and then finds it finished, and marks it as this
-- transaction's. Returns the event to refund.
CREATE FUNCTION claim_staff_refund(p_action uuid) RETURNS uuid
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, public, pg_temp AS $$
DECLARE a public.staff_actions%ROWTYPE;
BEGIN
  SELECT * INTO a FROM public.staff_actions WHERE id = p_action FOR UPDATE;
  IF NOT FOUND OR a.kind <> 'refund_event' OR a.organizer_id IS DISTINCT FROM public.app_scope() THEN
    RAISE EXCEPTION 'claim_staff_refund: no such action' USING ERRCODE = 'no_data_found';
  END IF;
  IF a.finished_at IS NOT NULL THEN
    RAISE EXCEPTION 'already_done' USING ERRCODE = '55000';
  END IF;
  IF NOT public.organizer_suspended(a.organizer_id) THEN
    RAISE EXCEPTION 'not_suspended' USING ERRCODE = '55000';
  END IF;
  UPDATE public.staff_actions SET claimed_xid = pg_catalog.pg_current_xact_id() WHERE id = p_action;
  RETURN a.event_id;
END $$;

-- The last step: records what the refund did, only in the transaction
-- that claimed the action.
CREATE FUNCTION finish_staff_refund(p_action uuid, p_result jsonb) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, public, pg_temp AS $$
BEGIN
  UPDATE public.staff_actions SET finished_at = pg_catalog.now(), result = p_result
   WHERE id = p_action AND organizer_id = public.app_scope() AND finished_at IS NULL
     AND claimed_xid = pg_catalog.pg_current_xact_id();
  IF NOT FOUND THEN
    RAISE EXCEPTION 'finish_staff_refund: not claimed in this transaction' USING ERRCODE = '42501';
  END IF;
END $$;

-- No new checkout or order for a suspended organizer, whatever the Go code
-- does. The Go side maps the message.
CREATE FUNCTION refuse_suspended_sales() RETURNS trigger LANGUAGE plpgsql
  SET search_path = pg_catalog, public, pg_temp AS $$
BEGIN
  IF public.organizer_suspended(NEW.organizer_id) THEN
    RAISE EXCEPTION 'organizer_suspended';
  END IF;
  RETURN NEW;
END $$;

-- 00002's body, plus: an owner or admin of a suspended organizer can't
-- start another one.
CREATE OR REPLACE FUNCTION create_organizer(p_name text, p_slug citext) RETURNS uuid
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, public, pg_temp AS $$
DECLARE org uuid;
        usr uuid := public.app_user();
BEGIN
  IF usr IS NULL THEN
    RAISE EXCEPTION 'create_organizer requires a user scope' USING ERRCODE = '42501';
  END IF;
  IF EXISTS (SELECT 1 FROM public.memberships m JOIN public.organizer_suspensions s ON s.organizer_id = m.organizer_id
              WHERE m.user_id = usr AND m.role IN ('owner', 'admin')) THEN
    RAISE EXCEPTION 'suspended_member' USING ERRCODE = '42501';
  END IF;
  INSERT INTO public.organizers(name, slug) VALUES (p_name, p_slug) RETURNING id INTO org;
  INSERT INTO public.memberships(organizer_id, user_id, role) VALUES (org, usr, 'owner');
  RETURN org;
END $$;
-- +goose StatementEnd
CREATE TRIGGER refuse_suspended_sales BEFORE INSERT ON checkouts FOR EACH ROW EXECUTE FUNCTION refuse_suspended_sales();
CREATE TRIGGER refuse_suspended_sales BEFORE INSERT ON orders FOR EACH ROW EXECUTE FUNCTION refuse_suspended_sales();
-- The storefront role learns of a suspension through the sale paths that
-- run as ts_system, and through the trigger; it gets no grant here.
REVOKE ALL ON FUNCTION organizer_suspended(uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION organizer_suspended(uuid) TO ts_app, ts_system;
REVOKE ALL ON FUNCTION staff_act(uuid, uuid, text, uuid, text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION staff_act(uuid, uuid, text, uuid, text) TO ts_app;
REVOKE ALL ON FUNCTION claim_staff_refund(uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION claim_staff_refund(uuid) TO ts_system;
REVOKE ALL ON FUNCTION finish_staff_refund(uuid, jsonb) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION finish_staff_refund(uuid, jsonb) TO ts_system;
REVOKE ALL ON FUNCTION refuse_suspended_sales() FROM PUBLIC;

-- +goose Down
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION create_organizer(p_name text, p_slug citext) RETURNS uuid
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, public, pg_temp AS $$
DECLARE org uuid;
        usr uuid := public.app_user();
BEGIN
  IF usr IS NULL THEN
    RAISE EXCEPTION 'create_organizer requires a user scope' USING ERRCODE = '42501';
  END IF;
  INSERT INTO public.organizers(name, slug) VALUES (p_name, p_slug) RETURNING id INTO org;
  INSERT INTO public.memberships(organizer_id, user_id, role) VALUES (org, usr, 'owner');
  RETURN org;
END $$;
-- +goose StatementEnd
DROP TRIGGER refuse_suspended_sales ON orders;
DROP TRIGGER refuse_suspended_sales ON checkouts;
DROP FUNCTION refuse_suspended_sales();
DROP FUNCTION finish_staff_refund(uuid, jsonb);
DROP FUNCTION claim_staff_refund(uuid);
DROP FUNCTION staff_act(uuid, uuid, text, uuid, text);
DROP FUNCTION organizer_suspended(uuid);
-- Fails while a 'platform' refund exists: those are money records, so the
-- rollback stops rather than rewrite or drop them.
ALTER TABLE refunds DROP CONSTRAINT refunds_reason_check,
  ADD CONSTRAINT refunds_reason_check CHECK (reason IN ('late_payment', 'organizer', 'event_cancelled'));
DROP TABLE organizer_suspensions, staff_actions;
