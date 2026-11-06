-- +goose Up
-- Where an order came from. 'online' is a checkout; 'comp' and 'manual' are
-- made by an owner or admin (created_by), with no payment row and no fee.
-- A manual order says how the organizer took the money.
ALTER TABLE orders
  ADD COLUMN source text NOT NULL DEFAULT 'online' CHECK (source IN ('online', 'comp', 'manual')),
  ADD COLUMN manual_method text CHECK (manual_method IN ('cash', 'cheque', 'invoice', 'other')),
  ADD COLUMN created_by uuid REFERENCES users ON DELETE SET NULL,
  ADD COLUMN buyer_name text NOT NULL DEFAULT '' CHECK (char_length(buyer_name) <= 120),
  ADD CONSTRAINT orders_manual_method CHECK ((source = 'manual') = (manual_method IS NOT NULL)),
  ADD CONSTRAINT orders_offline_no_fee CHECK (source = 'online' OR (fee_cents = 0 AND tax_cents = 0)),
  ADD CONSTRAINT orders_comp_free CHECK (source <> 'comp' OR total_cents = 0);
CREATE INDEX ON orders (created_by) WHERE created_by IS NOT NULL;

-- Those four are set at creation and never again, by any role: ts_system
-- has table-wide UPDATE on orders. The one change let through is the
-- creator going to NULL once their user row is gone (ON DELETE SET NULL).
-- Definer, so the users lookup works for roles that can't read users.
-- +goose StatementBegin
CREATE FUNCTION orders_source_fixed() RETURNS trigger LANGUAGE plpgsql SECURITY DEFINER
  SET search_path = pg_catalog, public, pg_temp AS $$
BEGIN
  IF NEW.source IS DISTINCT FROM OLD.source
     OR NEW.manual_method IS DISTINCT FROM OLD.manual_method
     OR NEW.buyer_name IS DISTINCT FROM OLD.buyer_name
     OR (NEW.created_by IS DISTINCT FROM OLD.created_by
         AND NOT (NEW.created_by IS NULL AND NOT EXISTS (SELECT 1 FROM public.users u WHERE u.id = OLD.created_by))) THEN
    RAISE EXCEPTION 'orders: source, method, creator and buyer name are set at creation' USING ERRCODE = 'insufficient_privilege';
  END IF;
  RETURN NEW;
END $$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION orders_source_fixed() FROM PUBLIC;
CREATE TRIGGER orders_source_fixed BEFORE UPDATE ON orders FOR EACH ROW EXECUTE FUNCTION orders_source_fixed();

-- Each owner or admin opts in to a mail per new online order.
ALTER TABLE memberships ADD COLUMN order_alerts boolean NOT NULL DEFAULT false;

-- Is p_user an owner or admin of the system transaction's organizer? The
-- caller is the system, so the check is on the member it names. Definer:
-- member_reads shows ts_system no memberships.
-- +goose StatementBegin
CREATE FUNCTION order_manager(p_user uuid) RETURNS boolean LANGUAGE sql STABLE SECURITY DEFINER
  SET search_path = pg_catalog, public, pg_temp AS $$
  SELECT EXISTS (SELECT 1 FROM public.memberships m
                  WHERE m.organizer_id = public.app_scope() AND m.user_id = p_user AND m.role IN ('owner', 'admin'))
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION order_manager(uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION order_manager(uuid) TO ts_system;

-- Team notes on an order: owners and admins only, plain text, never edited.
-- A delete clears the body and stamps deleted_at, by the author or an owner.
CREATE TABLE order_notes (
  id           uuid PRIMARY KEY DEFAULT uuidv7(),
  organizer_id uuid NOT NULL,
  order_id     uuid NOT NULL,
  author       uuid REFERENCES users ON DELETE SET NULL,
  body         text NOT NULL CHECK (char_length(body) <= 2000),
  created_at   timestamptz NOT NULL DEFAULT now(),
  deleted_at   timestamptz,
  FOREIGN KEY (order_id, organizer_id) REFERENCES orders (id, organizer_id),
  CHECK ((deleted_at IS NULL) = (body <> ''))
);
CREATE INDEX ON order_notes (order_id, created_at DESC);
CREATE INDEX ON order_notes (author) WHERE author IS NOT NULL;
ALTER TABLE order_notes ENABLE ROW LEVEL SECURITY;
-- Default privileges (00001) gave ts_app every right; keep only the columns.
REVOKE ALL ON order_notes FROM ts_app, ts_public, ts_system;
GRANT SELECT, INSERT (id, organizer_id, order_id, author, body), UPDATE (body, deleted_at) ON order_notes TO ts_app;
GRANT INSERT (id, organizer_id, order_id, author, body) ON order_notes TO ts_system;
-- Every ts_app policy names the scoped organizer, so a manager of one
-- organizer can't write a note into another's order.
CREATE POLICY order_notes_read ON order_notes FOR SELECT TO ts_app
  USING (organizer_id = (SELECT app_organizer())
         AND EXISTS (SELECT 1 FROM memberships m WHERE m.organizer_id = (SELECT app_organizer())
                       AND m.user_id = (SELECT app_user()) AND m.role IN ('owner', 'admin')));
CREATE POLICY order_notes_add ON order_notes FOR INSERT TO ts_app
  WITH CHECK (organizer_id = (SELECT app_organizer()) AND author = (SELECT app_user()) AND deleted_at IS NULL
              AND EXISTS (SELECT 1 FROM memberships m WHERE m.organizer_id = (SELECT app_organizer())
                            AND m.user_id = (SELECT app_user()) AND m.role IN ('owner', 'admin')));
CREATE POLICY order_notes_clear ON order_notes FOR UPDATE TO ts_app
  USING (organizer_id = (SELECT app_organizer()) AND deleted_at IS NULL
         AND EXISTS (SELECT 1 FROM memberships m WHERE m.organizer_id = (SELECT app_organizer())
                       AND m.user_id = (SELECT app_user())
                       AND (m.role = 'owner' OR m.role = 'admin' AND order_notes.author = (SELECT app_user()))))
  WITH CHECK (organizer_id = (SELECT app_organizer()) AND deleted_at IS NOT NULL AND body = '');
-- A comp or manual order's first note, written with the order.
CREATE POLICY order_notes_system ON order_notes FOR INSERT TO ts_system
  WITH CHECK (organizer_id = (SELECT app_scope()) AND public.order_manager(author));

-- Today's alert count per member per organizer (UTC days). Only
-- claim_order_alerts() reads or writes it.
CREATE TABLE order_alert_counts (
  organizer_id uuid NOT NULL REFERENCES organizers ON DELETE CASCADE,
  user_id      uuid NOT NULL REFERENCES users ON DELETE CASCADE,
  day          date NOT NULL,
  sent         int NOT NULL CHECK (sent BETWEEN 1 AND 50),
  PRIMARY KEY (organizer_id, user_id, day)
);
REVOKE ALL ON order_alert_counts FROM ts_app, ts_public, ts_system;
ALTER TABLE order_alert_counts ENABLE ROW LEVEL SECURITY;

-- The caller's own alert setting in the scoped organizer, owners and
-- admins only. False for anyone else, a key or staff included.
-- +goose StatementBegin
CREATE FUNCTION set_order_alerts(p_on boolean) RETURNS boolean LANGUAGE plpgsql SECURITY DEFINER
  SET search_path = pg_catalog, public, pg_temp AS $$
BEGIN
  UPDATE public.memberships m SET order_alerts = p_on
   WHERE m.organizer_id = public.app_organizer() AND m.user_id = public.app_user() AND m.role IN ('owner', 'admin');
  RETURN FOUND;
END $$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION set_order_alerts(boolean) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION set_order_alerts(boolean) TO ts_app;

-- One alert each for the scoped organizer's opted-in owners and admins,
-- counting toward 50 a UTC day: nth is this alert's place in the day, and
-- a member at 50 isn't returned. Nothing when no organizer is scoped.
-- +goose StatementBegin
CREATE FUNCTION claim_order_alerts() RETURNS TABLE(user_id uuid, nth int) LANGUAGE plpgsql SECURITY DEFINER
  SET search_path = pg_catalog, public, pg_temp AS $$
#variable_conflict use_column
DECLARE
  v_org uuid := public.app_scope();
  v_today date := (pg_catalog.statement_timestamp() AT TIME ZONE 'UTC')::date;
BEGIN
  IF v_org IS NULL THEN
    RETURN;
  END IF;
  DELETE FROM public.order_alert_counts c WHERE c.organizer_id = v_org AND c.day < v_today;
  RETURN QUERY
    INSERT INTO public.order_alert_counts AS c (organizer_id, user_id, day, sent)
    SELECT v_org, m.user_id, v_today, 1 FROM public.memberships m
     WHERE m.organizer_id = v_org AND m.role IN ('owner', 'admin') AND m.order_alerts
    ON CONFLICT (organizer_id, user_id, day) DO UPDATE SET sent = c.sent + 1 WHERE c.sent < 50
    RETURNING c.user_id, c.sent;
END $$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION claim_order_alerts() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION claim_order_alerts() TO ts_system;

-- Where an alert goes: the member's account email, only while they are an
-- opted-in owner or admin of the scoped organizer. NULL otherwise.
-- +goose StatementBegin
CREATE FUNCTION order_alert_recipient(p_user uuid) RETURNS text LANGUAGE sql STABLE SECURITY DEFINER
  SET search_path = pg_catalog, public, pg_temp AS $$
  SELECT u.email::text FROM public.users u JOIN public.memberships m ON m.user_id = u.id
   WHERE u.id = p_user AND m.organizer_id = public.app_scope() AND m.role IN ('owner', 'admin') AND m.order_alerts
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION order_alert_recipient(uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION order_alert_recipient(uuid) TO ts_system;

-- The system writes the audit row for a member's comp or manual order and
-- its first note, named as that member, in the order's transaction. Only
-- those two actions, on an order, for an owner or admin order_manager()
-- vouches for; never as staff or a key.
CREATE POLICY audit_system_member ON audit_log FOR INSERT TO ts_system
  WITH CHECK (organizer_id = (SELECT app_scope()) AND NOT actor_system AND actor_api_key_id IS NULL
              AND actor_staff_session_id IS NULL AND entity_type = 'order'
              AND action IN ('order.created_manual', 'order.note_added') AND public.order_manager(actor_user_id));

-- order_by_link, as in 00048, now also saying where the order came from
-- and, for a manual order, what its tickets came to: the valid ones, or with
-- none valid, the ones the last cancel voided (one statement voids them, so
-- they share released_at). That's what the organizer holds or invoiced.
DROP FUNCTION order_by_link(text, bytea);
-- +goose StatementBegin
CREATE FUNCTION order_by_link(p_code text, p_token_hash bytea)
RETURNS TABLE(code text, status text, email text, subtotal_cents bigint, discount_cents bigint, tax_cents bigint,
  fee_cents bigint, total_cents bigint, event_name text, timezone text, venue_name text, venue_address text,
  starts_at timestamptz, ends_at timestamptz, organizer_name text, event_status text, refunded_cents bigint,
  returned_cents bigint, names_until timestamptz, source text, manual_method text, manual_cents bigint, tickets jsonb)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog, public, pg_temp AS $$
  SELECT o.code, o.status, o.email::text, o.subtotal_cents, o.discount_cents, o.tax_cents, o.fee_cents, o.total_cents,
         e.name::text, e.timezone::text, e.venue_name, e.venue_address, e.starts_at, e.ends_at, g.name::text, e.status::text,
         coalesce((SELECT sum(r.amount_cents) FROM public.refunds r JOIN public.payments p ON p.id = r.payment_id
                    WHERE p.order_id = o.id AND r.status <> 'failed'), 0)::bigint,
         coalesce((SELECT sum(r.amount_cents) FROM public.refunds r JOIN public.payments p ON p.id = r.payment_id
                    WHERE p.order_id = o.id AND r.status = 'failed' AND r.returned_by_bank), 0)::bigint,
         CASE WHEN o.status IN ('paid', 'partially_refunded') AND e.status = 'live' AND NOT public.organizer_suspended(o.organizer_id)
                   AND statement_timestamp() < least(coalesce(e.names_locked_at, e.starts_at), e.starts_at)
              THEN least(coalesce(e.names_locked_at, e.starts_at), e.starts_at) END,
         o.source, o.manual_method,
         CASE WHEN o.source <> 'manual' THEN 0
              WHEN EXISTS (SELECT 1 FROM public.tickets t WHERE t.order_id = o.id AND t.status = 'valid')
              THEN (SELECT sum(t.charge_cents) FROM public.tickets t WHERE t.order_id = o.id AND t.status = 'valid')
              ELSE coalesce((SELECT sum(t.charge_cents) FROM public.tickets t WHERE t.order_id = o.id
                              AND t.released_at = (SELECT max(l.released_at) FROM public.tickets l WHERE l.order_id = o.id)), 0)
         END::bigint,
         coalesce((SELECT pg_catalog.jsonb_agg(pg_catalog.jsonb_build_object('id', t.id, 'ticket_type_name', tt.name,
                     'attendee_name', t.attendee_name, 'status', t.status, 'refunded', t.refund_id IS NOT NULL, 'name_changes', t.name_changes,
                     'secret', CASE WHEN t.status = 'valid' THEN pg_catalog.encode(t.secret, 'hex') END) ORDER BY t.id)
                   FROM public.tickets t JOIN public.ticket_types tt ON tt.id = t.ticket_type_id
                  WHERE t.order_id = o.id), '[]'::jsonb)
    FROM public.orders o
    JOIN public.events e ON e.id = o.event_id
    JOIN public.organizers g ON g.id = o.organizer_id
   WHERE o.code = pg_catalog.upper(p_code) AND o.magic_token_hash = p_token_hash
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION order_by_link(text, bytea) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION order_by_link(text, bytea) TO ts_app;

-- +goose Down
-- order_by_link back to 00048.
DROP FUNCTION order_by_link(text, bytea);
-- +goose StatementBegin
CREATE FUNCTION order_by_link(p_code text, p_token_hash bytea)
RETURNS TABLE(code text, status text, email text, subtotal_cents bigint, discount_cents bigint, tax_cents bigint,
  fee_cents bigint, total_cents bigint, event_name text, timezone text, venue_name text, venue_address text,
  starts_at timestamptz, ends_at timestamptz, organizer_name text, event_status text, refunded_cents bigint,
  returned_cents bigint, names_until timestamptz, tickets jsonb)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog, public, pg_temp AS $$
  SELECT o.code, o.status, o.email::text, o.subtotal_cents, o.discount_cents, o.tax_cents, o.fee_cents, o.total_cents,
         e.name::text, e.timezone::text, e.venue_name, e.venue_address, e.starts_at, e.ends_at, g.name::text, e.status::text,
         coalesce((SELECT sum(r.amount_cents) FROM public.refunds r JOIN public.payments p ON p.id = r.payment_id
                    WHERE p.order_id = o.id AND r.status <> 'failed'), 0)::bigint,
         coalesce((SELECT sum(r.amount_cents) FROM public.refunds r JOIN public.payments p ON p.id = r.payment_id
                    WHERE p.order_id = o.id AND r.status = 'failed' AND r.returned_by_bank), 0)::bigint,
         CASE WHEN o.status IN ('paid', 'partially_refunded') AND e.status = 'live' AND NOT public.organizer_suspended(o.organizer_id)
                   AND statement_timestamp() < least(coalesce(e.names_locked_at, e.starts_at), e.starts_at)
              THEN least(coalesce(e.names_locked_at, e.starts_at), e.starts_at) END,
         coalesce((SELECT pg_catalog.jsonb_agg(pg_catalog.jsonb_build_object('id', t.id, 'ticket_type_name', tt.name,
                     'attendee_name', t.attendee_name, 'status', t.status, 'refunded', t.refund_id IS NOT NULL, 'name_changes', t.name_changes,
                     'secret', CASE WHEN t.status = 'valid' THEN pg_catalog.encode(t.secret, 'hex') END) ORDER BY t.id)
                   FROM public.tickets t JOIN public.ticket_types tt ON tt.id = t.ticket_type_id
                  WHERE t.order_id = o.id), '[]'::jsonb)
    FROM public.orders o
    JOIN public.events e ON e.id = o.event_id
    JOIN public.organizers g ON g.id = o.organizer_id
   WHERE o.code = pg_catalog.upper(p_code) AND o.magic_token_hash = p_token_hash
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION order_by_link(text, bytea) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION order_by_link(text, bytea) TO ts_app;
DROP POLICY audit_system_member ON audit_log;
DROP FUNCTION order_alert_recipient(uuid);
DROP FUNCTION claim_order_alerts();
DROP FUNCTION set_order_alerts(boolean);
DROP TABLE order_alert_counts;
DROP TABLE order_notes;
DROP FUNCTION order_manager(uuid);
ALTER TABLE memberships DROP COLUMN order_alerts;
DROP TRIGGER orders_source_fixed ON orders;
DROP FUNCTION orders_source_fixed();
ALTER TABLE orders DROP CONSTRAINT orders_comp_free, DROP CONSTRAINT orders_offline_no_fee, DROP CONSTRAINT orders_manual_method,
  DROP COLUMN buyer_name, DROP COLUMN created_by, DROP COLUMN manual_method, DROP COLUMN source;
