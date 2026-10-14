-- +goose Up
-- A buyer's cart. The token is the buyer's only key to it; we keep its hash.
CREATE TABLE checkouts (
  id            uuid PRIMARY KEY DEFAULT uuidv7(),
  organizer_id  uuid NOT NULL,
  event_id      uuid NOT NULL,
  token_hash    bytea NOT NULL UNIQUE CHECK (length(token_hash) = 32),
  promo_code_id uuid,
  status        text NOT NULL DEFAULT 'open' CHECK (status IN ('open', 'paying', 'completed')),
  expires_at    timestamptz NOT NULL,
  extended_at   timestamptz,
  created_at    timestamptz NOT NULL DEFAULT now(),
  FOREIGN KEY (event_id, organizer_id) REFERENCES events (id, organizer_id) ON DELETE CASCADE,
  FOREIGN KEY (promo_code_id, event_id) REFERENCES promo_codes (id, event_id) ON DELETE SET NULL (promo_code_id),
  UNIQUE (id, organizer_id),
  UNIQUE (id, event_id)
);
CREATE INDEX ON checkouts (event_id, expires_at) WHERE status <> 'completed';
CREATE INDEX ON checkouts (promo_code_id) WHERE status <> 'completed';
CREATE INDEX ON checkouts (expires_at) WHERE status <> 'completed';

CREATE TABLE holds (
  id             uuid PRIMARY KEY DEFAULT uuidv7(),
  organizer_id   uuid NOT NULL,
  event_id       uuid NOT NULL,
  checkout_id    uuid NOT NULL,
  ticket_type_id uuid NOT NULL,
  quantity       int NOT NULL CHECK (quantity BETWEEN 1 AND 100),
  FOREIGN KEY (checkout_id, organizer_id) REFERENCES checkouts (id, organizer_id) ON DELETE CASCADE,
  FOREIGN KEY (checkout_id, event_id) REFERENCES checkouts (id, event_id) ON DELETE CASCADE,
  -- Cascade: the catalogue refuses to delete a ticket type that an open
  -- checkout holds (under the quota locks), so only expired holds go with it.
  FOREIGN KEY (ticket_type_id, event_id) REFERENCES ticket_types (id, event_id) ON DELETE CASCADE,
  UNIQUE (checkout_id, ticket_type_id)
);
CREATE INDEX ON holds (ticket_type_id);

CREATE TABLE orders (
  id               uuid PRIMARY KEY DEFAULT uuidv7(),
  organizer_id     uuid NOT NULL,
  event_id         uuid NOT NULL,
  checkout_id      uuid UNIQUE,
  code             text NOT NULL UNIQUE CHECK (code ~ '^TS-[0-9A-HJKMNP-TV-Z]{5}$'),
  email            citext NOT NULL CHECK (length(email) BETWEEN 3 AND 254),
  status           text NOT NULL DEFAULT 'pending'
                   CHECK (status IN ('pending', 'paid', 'cancelled', 'refunded', 'partially_refunded')),
  currency         char(3) NOT NULL DEFAULT 'USD' CHECK (currency = 'USD'),
  subtotal_cents   bigint NOT NULL CHECK (subtotal_cents >= 0),
  discount_cents   bigint NOT NULL CHECK (discount_cents >= 0),
  tax_cents        bigint NOT NULL CHECK (tax_cents >= 0),
  fee_cents        bigint NOT NULL CHECK (fee_cents >= 0),
  total_cents      bigint NOT NULL CHECK (total_cents >= 0),
  fee_mode         text NOT NULL CHECK (fee_mode IN ('absorb', 'pass')),
  promo_code_id    uuid,
  quote            jsonb NOT NULL,  -- pricing.Quote at pay time; tickets are cut from it
  details          jsonb NOT NULL,  -- attendee names and answers until confirmation
  magic_token_hash bytea,
  created_at       timestamptz NOT NULL DEFAULT now(),
  paid_at          timestamptz,
  FOREIGN KEY (event_id, organizer_id) REFERENCES events (id, organizer_id),
  FOREIGN KEY (checkout_id, organizer_id) REFERENCES checkouts (id, organizer_id) ON DELETE SET NULL (checkout_id),
  FOREIGN KEY (promo_code_id, event_id) REFERENCES promo_codes (id, event_id) ON DELETE SET NULL (promo_code_id),
  UNIQUE (id, organizer_id),
  UNIQUE (id, event_id)
);
CREATE INDEX ON orders (event_id, created_at DESC);
CREATE INDEX ON orders (promo_code_id) WHERE promo_code_id IS NOT NULL;

CREATE TABLE tickets (
  id             uuid PRIMARY KEY DEFAULT uuidv7(),
  organizer_id   uuid NOT NULL,
  event_id       uuid NOT NULL,
  order_id       uuid NOT NULL,
  ticket_type_id uuid NOT NULL,
  price_cents    bigint NOT NULL CHECK (price_cents >= 0),
  discount_cents bigint NOT NULL CHECK (discount_cents >= 0),
  tax_cents      bigint NOT NULL CHECK (tax_cents >= 0),
  fee_cents      bigint NOT NULL CHECK (fee_cents >= 0),
  attendee_name  text NOT NULL DEFAULT '' CHECK (char_length(attendee_name) <= 120),
  secret         bytea NOT NULL UNIQUE CHECK (length(secret) = 16),
  status         text NOT NULL DEFAULT 'valid' CHECK (status IN ('valid', 'void')),
  checked_in_at  timestamptz,
  created_at     timestamptz NOT NULL DEFAULT now(),
  FOREIGN KEY (event_id, organizer_id) REFERENCES events (id, organizer_id),
  FOREIGN KEY (order_id, organizer_id) REFERENCES orders (id, organizer_id),
  FOREIGN KEY (order_id, event_id) REFERENCES orders (id, event_id),
  FOREIGN KEY (ticket_type_id, event_id) REFERENCES ticket_types (id, event_id),
  UNIQUE (id, organizer_id)
);
CREATE INDEX ON tickets (ticket_type_id) WHERE status = 'valid';
CREATE INDEX ON tickets (event_id, ticket_type_id);
CREATE INDEX ON tickets (order_id);

CREATE TABLE answers (
  id           uuid PRIMARY KEY DEFAULT uuidv7(),
  organizer_id uuid NOT NULL,
  event_id     uuid NOT NULL,
  order_id     uuid NOT NULL,
  ticket_id    uuid,              -- NULL: an order-scope answer
  question_id  uuid NOT NULL,
  value        text NOT NULL CHECK (char_length(value) <= 1000),
  FOREIGN KEY (order_id, organizer_id) REFERENCES orders (id, organizer_id) ON DELETE CASCADE,
  FOREIGN KEY (order_id, event_id) REFERENCES orders (id, event_id) ON DELETE CASCADE,
  FOREIGN KEY (ticket_id, organizer_id) REFERENCES tickets (id, organizer_id) ON DELETE CASCADE,
  FOREIGN KEY (question_id, event_id) REFERENCES questions (id, event_id)
);
CREATE INDEX ON answers (order_id);
CREATE INDEX ON answers (question_id);

CREATE TABLE payments (
  id                    uuid PRIMARY KEY DEFAULT uuidv7(),
  organizer_id          uuid NOT NULL,
  order_id              uuid NOT NULL UNIQUE,
  provider              text NOT NULL CHECK (provider IN ('stripe', 'test')),
  stripe_account_id     text NOT NULL,
  payment_intent_id     text NOT NULL UNIQUE,
  client_secret         text NOT NULL,
  amount_cents          bigint NOT NULL CHECK (amount_cents >= 50),
  application_fee_cents bigint NOT NULL CHECK (application_fee_cents >= 0),
  status                text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'succeeded', 'failed')),
  created_at            timestamptz NOT NULL DEFAULT now(),
  updated_at            timestamptz NOT NULL DEFAULT now(),
  FOREIGN KEY (order_id, organizer_id) REFERENCES orders (id, organizer_id),
  UNIQUE (id, organizer_id)
);

CREATE TABLE refunds (
  id               uuid PRIMARY KEY DEFAULT uuidv7(),
  organizer_id     uuid NOT NULL,
  payment_id       uuid NOT NULL,
  amount_cents     bigint NOT NULL CHECK (amount_cents > 0),
  fee_refunded     boolean NOT NULL,
  stripe_refund_id text UNIQUE,
  status           text NOT NULL CHECK (status IN ('requested', 'succeeded', 'failed')),
  reason           text NOT NULL CHECK (reason IN ('late_payment', 'organizer', 'event_cancelled')),
  actor_user_id    uuid REFERENCES users ON DELETE SET NULL,
  created_at       timestamptz NOT NULL DEFAULT now(),
  updated_at       timestamptz NOT NULL DEFAULT now(),
  FOREIGN KEY (payment_id, organizer_id) REFERENCES payments (id, organizer_id)
);

-- Stripe event ids we have processed. Not tenant data: an id and a type.
CREATE TABLE stripe_events (
  id          text PRIMARY KEY,
  type        text NOT NULL,
  received_at timestamptz NOT NULL DEFAULT now()
);

-- Members read their organizer's sales; nothing in the app role writes them.
-- Sales are written by ts_system only (P3b grants what refunds need).
ALTER TABLE checkouts ENABLE ROW LEVEL SECURITY;
ALTER TABLE holds ENABLE ROW LEVEL SECURITY;
ALTER TABLE orders ENABLE ROW LEVEL SECURITY;
ALTER TABLE tickets ENABLE ROW LEVEL SECURITY;
ALTER TABLE answers ENABLE ROW LEVEL SECURITY;
ALTER TABLE payments ENABLE ROW LEVEL SECURITY;
ALTER TABLE refunds ENABLE ROW LEVEL SECURITY;
CREATE POLICY checkouts_scope ON checkouts USING (organizer_id = (SELECT app_organizer())) WITH CHECK (organizer_id = (SELECT app_organizer()));
CREATE POLICY holds_scope ON holds USING (organizer_id = (SELECT app_organizer())) WITH CHECK (organizer_id = (SELECT app_organizer()));
CREATE POLICY orders_scope ON orders USING (organizer_id = (SELECT app_organizer())) WITH CHECK (organizer_id = (SELECT app_organizer()));
CREATE POLICY tickets_scope ON tickets USING (organizer_id = (SELECT app_organizer())) WITH CHECK (organizer_id = (SELECT app_organizer()));
CREATE POLICY answers_scope ON answers USING (organizer_id = (SELECT app_organizer())) WITH CHECK (organizer_id = (SELECT app_organizer()));
CREATE POLICY payments_scope ON payments USING (organizer_id = (SELECT app_organizer())) WITH CHECK (organizer_id = (SELECT app_organizer()));
CREATE POLICY refunds_scope ON refunds USING (organizer_id = (SELECT app_organizer())) WITH CHECK (organizer_id = (SELECT app_organizer()));
REVOKE INSERT, UPDATE, DELETE ON checkouts, holds, orders, tickets, answers, payments, refunds, stripe_events FROM ts_app;
-- The client secret is for the buyer's checkout only, never the dashboard.
REVOKE SELECT ON payments FROM ts_app;
GRANT SELECT (id, organizer_id, order_id, provider, stripe_account_id, payment_intent_id, amount_cents,
  application_fee_cents, status, created_at, updated_at) ON payments TO ts_app;

CREATE POLICY checkouts_system ON checkouts TO ts_system USING (organizer_id = (SELECT app_scope())) WITH CHECK (organizer_id = (SELECT app_scope()));
CREATE POLICY holds_system ON holds TO ts_system USING (organizer_id = (SELECT app_scope())) WITH CHECK (organizer_id = (SELECT app_scope()));
CREATE POLICY orders_system ON orders TO ts_system USING (organizer_id = (SELECT app_scope())) WITH CHECK (organizer_id = (SELECT app_scope()));
CREATE POLICY tickets_system ON tickets TO ts_system USING (organizer_id = (SELECT app_scope())) WITH CHECK (organizer_id = (SELECT app_scope()));
CREATE POLICY answers_system ON answers TO ts_system USING (organizer_id = (SELECT app_scope())) WITH CHECK (organizer_id = (SELECT app_scope()));
CREATE POLICY payments_system ON payments TO ts_system USING (organizer_id = (SELECT app_scope())) WITH CHECK (organizer_id = (SELECT app_scope()));
CREATE POLICY refunds_system ON refunds TO ts_system USING (organizer_id = (SELECT app_scope())) WITH CHECK (organizer_id = (SELECT app_scope()));
GRANT SELECT, INSERT, UPDATE, DELETE ON checkouts, holds TO ts_system;
GRANT SELECT, INSERT, UPDATE ON orders, tickets, payments, refunds TO ts_system;
GRANT SELECT, INSERT ON answers, stripe_events TO ts_system;

-- What a quota has given out: valid tickets plus what open checkouts hold
-- right now, except the checkout named by p_except. Runs with the caller's
-- rights, so RLS keeps it inside the caller's organizer.
CREATE FUNCTION quota_used(p_quota uuid, p_except uuid) RETURNS bigint LANGUAGE sql STABLE
  SET search_path = pg_catalog, public, pg_temp AS $$
  SELECT (SELECT count(*) FROM public.tickets t
            JOIN public.quota_ticket_types x ON x.ticket_type_id = t.ticket_type_id
           WHERE x.quota_id = p_quota AND t.status = 'valid')
       + (SELECT coalesce(sum(h.quantity), 0) FROM public.holds h
            JOIN public.checkouts c ON c.id = h.checkout_id
            JOIN public.quota_ticket_types x ON x.ticket_type_id = h.ticket_type_id
           WHERE x.quota_id = p_quota AND c.status IN ('open', 'paying') AND c.expires_at > now()
             AND c.id IS DISTINCT FROM p_except) $$;
REVOKE ALL ON FUNCTION quota_used(uuid, uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION quota_used(uuid, uuid) TO ts_app, ts_system;

-- How many of each ticket type of a live event can still be sold.
-- +goose StatementBegin
CREATE FUNCTION event_availability(p_event uuid) RETURNS TABLE(ticket_type_id uuid, remaining bigint)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog, public, pg_temp AS $$
  SELECT t.id, greatest(0, min(q.size - public.quota_used(q.id, NULL)))
    FROM public.ticket_types t
    JOIN public.events e ON e.id = t.event_id AND e.status = 'live'
    JOIN public.quota_ticket_types x ON x.ticket_type_id = t.id
    JOIN public.quotas q ON q.id = x.quota_id
   WHERE t.event_id = p_event
   GROUP BY t.id
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION event_availability(uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION event_availability(uuid) TO ts_public, ts_system;

-- A buyer's token and an order code are the only keys a buyer has. These
-- name the organizer to scope to, and nothing else.
CREATE FUNCTION checkout_scope(p_token_hash bytea) RETURNS uuid LANGUAGE sql STABLE SECURITY DEFINER
  SET search_path = pg_catalog, public, pg_temp AS
  $$ SELECT organizer_id FROM public.checkouts WHERE token_hash = p_token_hash $$;
CREATE FUNCTION order_scope(p_code text) RETURNS uuid LANGUAGE sql STABLE SECURITY DEFINER
  SET search_path = pg_catalog, public, pg_temp AS
  $$ SELECT organizer_id FROM public.orders WHERE code = upper(p_code) $$;
-- Abandoned carts older than a week; one with a pending order may still be
-- paid late, so those stay until the order settles.
-- +goose StatementBegin
CREATE FUNCTION purge_expired_checkouts() RETURNS bigint LANGUAGE sql SECURITY DEFINER
  SET search_path = pg_catalog, public, pg_temp AS $$
  WITH gone AS (
    DELETE FROM public.checkouts c
     WHERE c.status <> 'completed' AND c.expires_at < now() - interval '7 days'
       AND NOT EXISTS (SELECT 1 FROM public.orders o WHERE o.checkout_id = c.id AND o.status = 'pending')
    RETURNING 1)
  SELECT count(*) FROM gone
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION checkout_scope(bytea), order_scope(text), purge_expired_checkouts() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION checkout_scope(bytea), order_scope(text), purge_expired_checkouts() TO ts_app;

-- +goose Down
DROP FUNCTION purge_expired_checkouts(), order_scope(text), checkout_scope(bytea), event_availability(uuid), quota_used(uuid, uuid);
DROP TABLE stripe_events, refunds, payments, answers, tickets, orders, holds, checkouts;
