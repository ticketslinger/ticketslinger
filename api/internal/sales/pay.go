package sales

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"log/slog"
	"net/mail"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ticketslinger/ticketslinger/api/internal/auth"
	"github.com/ticketslinger/ticketslinger/api/internal/catalogue"
	"github.com/ticketslinger/ticketslinger/api/internal/plugin"
	"github.com/ticketslinger/ticketslinger/api/internal/pricing"
	"github.com/ticketslinger/ticketslinger/api/internal/system"
)

type Answer struct {
	QuestionID  uuid.UUID `json:"question_id"`
	TicketIndex int       `json:"ticket_index"` // -1: the order; else 0-based over the checkout's tickets in item order
	Value       string    `json:"value"`
}

type Details struct {
	Email     string   `json:"email"`
	Attendees []string `json:"attendees"` // optional names, one per ticket in item order
	Answers   []Answer `json:"answers"`
}

type PaymentStart struct {
	Kind           string // "stripe" | "test" | "free"
	ClientSecret   string
	PublishableKey string
	Account        string // for Stripe.js's stripeAccount option
	AmountCents    int64
	OrderCode      string // set for "free"; the order is already paid
	OrderURL       string // set for "free"
}

// payment is the intent an order already has.
type payment struct{ IntentID, ClientSecret, Account string }

// pendingOrder is what Pay needs from the order it wrote.
type pendingOrder struct {
	ID                   uuid.UUID
	Code, Account        string
	TotalCents, FeeCents int64
	Tickets              int
	Payment              *payment // set when an earlier call made the intent
}

// Pay takes the buyer's details and starts paying: one pending order per
// checkout and one PaymentIntent per order, however often it is called. A
// free order is paid at once.
//
// The client secret is read back from payments as ts_system, the only role
// that can see that column; the dashboard's role never can.
func (s *Service) Pay(ctx context.Context, token string, d Details) (PaymentStart, error) {
	d.Email = strings.TrimSpace(d.Email)
	if !goodEmail(d.Email) {
		return PaymentStart{}, errBadEmail
	}
	org, err := s.checkoutOrg(ctx, token)
	if err != nil {
		return PaymentStart{}, err
	}
	var order pendingOrder
	var free PaymentStart
	var event uuid.UUID
	err = system.Run(ctx, s.Pool, org, func(tx pgx.Tx) error {
		c, err := lockCheckout(ctx, tx, HashToken(token))
		if err != nil {
			return err
		}
		event = c.event
		items, q, err := s.cart(ctx, tx, c.id, c.event, c.promo, c.feeMode)
		if err != nil {
			return err
		}
		clean, err := checkDetails(ctx, tx, c.event, items, d)
		if err != nil {
			return err
		}
		order, err = upsertPendingOrder(ctx, tx, org, c, q, clean)
		if err != nil {
			return err
		}
		// Every decision from here on is the stored order's, the amount the
		// intent was (or will be) made for. Refusing rolls the order back.
		if order.TotalCents > 0 && order.TotalCents < pricing.MinimumChargeCents {
			return errBelowMinimum
		}
		if order.TotalCents > 0 {
			// Stripe can switch charges off after the hold (account.updated).
			if order.Account, err = s.paymentsReady(ctx, tx, org); err != nil {
				return err
			}
		}
		for _, it := range items {
			order.Tickets += it.Quantity
		}
		if _, err := tx.Exec(ctx, `update checkouts set status = 'paying',
			expires_at = case when extended_at is null then expires_at + interval '5 minutes' else expires_at end,
			extended_at = coalesce(extended_at, statement_timestamp()) where id = $1`, c.id); err != nil {
			return err
		}
		if order.TotalCents == 0 {
			if _, err := s.confirmOrder(ctx, tx, org, order.ID); err != nil {
				return err
			}
			free = PaymentStart{Kind: "free", OrderCode: order.Code, OrderURL: s.orderURL(order.Code, order.ID, 0)}
		}
		return nil
	})
	if err != nil || free.Kind == "free" {
		return free, err
	}
	if order.Payment != nil {
		return s.start(order, *order.Payment), nil
	}
	// Outside the transaction: never hold quota or checkout locks across a network call.
	pi, err := s.Payments.CreatePaymentIntent(ctx, plugin.PaymentIntentInput{
		Account: order.Account, AmountCents: order.TotalCents, ApplicationFeeCents: order.FeeCents,
		Description:    "Order " + order.Code + ": " + plural(order.Tickets, "ticket"), // stable, like the key
		IdempotencyKey: "pi-" + order.ID.String(),
		Metadata:       map[string]string{"order_id": order.ID.String(), "order_code": order.Code},
	})
	if err != nil {
		slog.ErrorContext(ctx, "payment intent", "order", order.ID, "err", err)
		if errors.Is(err, plugin.ErrRefused) {
			return PaymentStart{}, ErrPaymentsRefused
		}
		return PaymentStart{}, ErrPaymentsUnavailable
	}
	// A Pay running alongside may have stored its intent first: answer with
	// whatever the row holds, so every caller gets the same payment.
	var p payment
	err = system.Run(ctx, s.Pool, org, func(tx pgx.Tx) error {
		// The event is still on sale, and stays so until this commits: a
		// cancellation (FOR NO KEY UPDATE) waits, and its cancel_payment job
		// then finds this intent stored. One that got in first is seen here.
		// Nothing else is held, so this adds no lock-order edge.
		var status string
		if err := tx.QueryRow(ctx, `select status from events where id = $1 for share`, event).Scan(&status); err != nil {
			return err
		}
		if status == "cancelled" {
			return errEventCancelled
		}
		if status != "live" {
			return catalogue.ErrNotFound
		}
		err := tx.QueryRow(ctx, `insert into payments (organizer_id, order_id, provider, stripe_account_id, payment_intent_id, client_secret, amount_cents, application_fee_cents)
			values ($1, $2, $3, $4, $5, $6, $7, $8) on conflict (order_id) do nothing
			returning payment_intent_id, client_secret, stripe_account_id`,
			org, order.ID, s.Payments.Name(), pi.Account, pi.ID, pi.ClientSecret, order.TotalCents, order.FeeCents).Scan(&p.IntentID, &p.ClientSecret, &p.Account)
		if errors.Is(err, pgx.ErrNoRows) {
			err = tx.QueryRow(ctx, `select payment_intent_id, client_secret, stripe_account_id from payments where order_id = $1`, order.ID).
				Scan(&p.IntentID, &p.ClientSecret, &p.Account)
		}
		return err
	})
	if errors.Is(err, errEventCancelled) || errors.Is(err, catalogue.ErrNotFound) {
		// Taken off sale while the intent was made: nobody gets to pay it.
		if cerr := s.Payments.CancelPaymentIntent(ctx, pi.Account, pi.ID, "cancel-"+order.ID.String()); cerr != nil {
			slog.WarnContext(ctx, "cancelling an intent made as its event went off sale", "order", order.ID, "intent", pi.ID, "err", cerr)
		}
	}
	if err != nil {
		return PaymentStart{}, err
	}
	return s.start(order, p), nil
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return strconv.Itoa(n) + " " + word + "s"
}

func (s *Service) start(o pendingOrder, p payment) PaymentStart {
	return PaymentStart{Kind: s.Payments.Name(), ClientSecret: p.ClientSecret, PublishableKey: s.Payments.PublishableKey(),
		Account: p.Account, AmountCents: o.TotalCents}
}

// paymentsReady returns the organizer's Stripe account if it can take cards:
// connected, not disconnected, and Stripe has charges on. In direct mode
// every charge is the server's own: "" (no Stripe-Account), and always ready.
func (s *Service) paymentsReady(ctx context.Context, tx pgx.Tx, org uuid.UUID) (string, error) {
	if s.Payments == nil {
		return "", ErrPaymentsNotConfigured
	}
	if s.Direct {
		return "", nil
	}
	var account *string
	var enabled bool
	// A disconnected organizer keeps the old id for its past payments.
	// An unlocked read on purpose: connect and disconnect hold the organizer
	// row across Stripe calls, and a sale (holding quota locks) must never
	// wait on Stripe. The race this leaves is accepted: a Pay that read
	// "ready" just before a disconnect commits makes its intent anyway. On a
	// linked account Stripe refuses it (our access is gone); on a created
	// one at most that one sale lands in the organizer's own account, and
	// its webhook still finds the order because the account id stays.
	if err := tx.QueryRow(ctx, `select stripe_account_id, stripe_charges_enabled and stripe_disconnected_at is null
		from organizers where id = $1`, org).Scan(&account, &enabled); err != nil {
		return "", err
	}
	if account == nil || !enabled {
		return "", errPaymentsNotReady
	}
	return *account, nil
}

// goodEmail: a bare address (no display name) with a dot in its domain, as
// the orders table stores it.
func goodEmail(e string) bool {
	a, err := mail.ParseAddress(e)
	if err != nil || a.Address != e || len(e) > 254 {
		return false
	}
	_, domain, _ := strings.Cut(e, "@")
	return strings.Contains(strings.Trim(domain, "."), ".")
}

type lockedCheckout struct {
	id, event uuid.UUID
	promo     *promo
	feeMode   string
}

// lockCheckout locks the open checkout with this token hash, of an event
// still on sale.
func lockCheckout(ctx context.Context, tx pgx.Tx, hash []byte) (lockedCheckout, error) {
	var c lockedCheckout
	var p promo
	var promoID *uuid.UUID
	var status, code, event string
	var expired bool
	err := tx.QueryRow(ctx, `select c.id, c.event_id, c.status, c.expires_at <= statement_timestamp(), e.status, c.promo_code_id,
			coalesce(p.code, ''), coalesce(p.kind, ''), coalesce(p.value, 0), e.fee_mode,
			coalesce((select o.code from orders o where o.checkout_id = c.id), '')
		from checkouts c join events e on e.id = c.event_id left join promo_codes p on p.id = c.promo_code_id
		where c.token_hash = $1 for update of c`, hash).
		Scan(&c.id, &c.event, &status, &expired, &event, &promoID, &p.code, &p.kind, &p.value, &c.feeMode, &code)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return c, catalogue.ErrNotFound // purged since checkout_scope looked
	case err != nil:
		return c, err
	case status == "completed":
		return c, CheckoutCompleted{OrderCode: code}
	case event == "cancelled":
		return c, errEventCancelled
	case event != "live": // taken off sale since the hold
		return c, catalogue.ErrNotFound
	case expired:
		return c, ErrCheckoutExpired
	}
	if promoID != nil {
		p.id = *promoID
		c.promo = &p
	}
	return c, nil
}

// cart reads a checkout's items in item order and prices them as they
// stand. Prices can't move while a checkout holds them (the catalogue's guards).
func (s *Service) cart(ctx context.Context, tx pgx.Tx, checkout, event uuid.UUID, p *promo, feeMode string) ([]Item, pricing.Quote, error) {
	rows, _ := tx.Query(ctx, `select ticket_type_id, quantity from holds where checkout_id = $1 order by id`, checkout)
	items, err := pgx.CollectRows(rows, pgx.RowToStructByPos[Item])
	if err != nil {
		return nil, pricing.Quote{}, err
	}
	tts, err := ticketTypes(ctx, tx, event, itemIDs(items), p)
	if err != nil {
		return nil, pricing.Quote{}, err
	}
	return items, pricing.Compute(lines(tts, items, p), p.pricing(), feeMode, s.Fee), nil
}

type question struct {
	id                     uuid.UUID
	label, kind, scope     string
	options                []string
	required, appliesToAll bool
	appliesTo              []uuid.UUID
}

func (q question) asks(tt uuid.UUID) bool { return q.appliesToAll || slices.Contains(q.appliesTo, tt) }

const maxAnswer = 1000

// checkDetails checks the attendee names and answers against the event's
// questions and the checkout's tickets, and returns them cleaned: trimmed,
// with blank answers dropped.
func checkDetails(ctx context.Context, tx pgx.Tx, event uuid.UUID, items []Item, d Details) (Details, error) {
	var tickets []uuid.UUID // ticket type of each ticket, in item order
	for _, it := range items {
		for range it.Quantity {
			tickets = append(tickets, it.TicketTypeID)
		}
	}
	out := Details{Email: d.Email, Attendees: []string{}, Answers: []Answer{}}
	if len(d.Attendees) > len(tickets) {
		return out, errAttendees
	}
	for _, a := range d.Attendees {
		a = strings.TrimSpace(a)
		if utf8.RuneCountInString(a) > 120 {
			return out, errAttendees
		}
		out.Attendees = append(out.Attendees, a)
	}
	rows, _ := tx.Query(ctx, `select q.id, q.label, q.kind, q.scope, array(select jsonb_array_elements_text(q.options)), q.required, q.applies_to_all,
			array(select a.ticket_type_id from question_applies_to a where a.question_id = q.id)
		from questions q where q.event_id = $1 order by q.position, q.id`, event)
	var qs []question
	var q question
	if _, err := pgx.ForEachRow(rows, []any{&q.id, &q.label, &q.kind, &q.scope, &q.options, &q.required, &q.appliesToAll, &q.appliesTo}, func() error {
		qs = append(qs, q)
		q = question{}
		return nil
	}); err != nil {
		return out, err
	}
	asked := func(q question, ticket int) bool {
		if q.scope == "order" {
			return ticket == -1 && slices.ContainsFunc(tickets, q.asks)
		}
		return ticket >= 0 && ticket < len(tickets) && q.asks(tickets[ticket])
	}
	type key struct {
		q      uuid.UUID
		ticket int
	}
	given := map[key]string{}
	for _, a := range d.Answers {
		i := slices.IndexFunc(qs, func(q question) bool { return q.id == a.QuestionID })
		if i < 0 {
			return out, errBadAnswer(a.QuestionID, a.TicketIndex, "That question isn't part of this event. Refresh the page and answer again.")
		}
		q := qs[i]
		if !asked(q, a.TicketIndex) {
			return out, errBadAnswer(q.id, a.TicketIndex, "“"+q.label+"” isn't asked of that ticket.")
		}
		k := key{q.id, a.TicketIndex}
		if _, dup := given[k]; dup {
			return out, errBadAnswer(q.id, a.TicketIndex, "“"+q.label+"” is answered twice for the same ticket. Send one answer.")
		}
		v := strings.TrimSpace(a.Value)
		given[k] = v
		if v == "" {
			continue
		}
		switch {
		case utf8.RuneCountInString(v) > maxAnswer:
			return out, errBadAnswer(q.id, a.TicketIndex, "Keep answers to 1,000 characters.")
		case q.kind == "select" && !slices.Contains(q.options, v):
			return out, errBadAnswer(q.id, a.TicketIndex, "Pick one of the choices for “"+q.label+"”.")
		case q.kind == "checkbox" && v != "true" && v != "false":
			return out, errBadAnswer(q.id, a.TicketIndex, "“"+q.label+"” takes true or false.")
		}
		out.Answers = append(out.Answers, Answer{QuestionID: q.id, TicketIndex: a.TicketIndex, Value: v})
	}
	// A required checkbox has to be ticked: that's what makes it required.
	answered := func(q question, ticket int) bool {
		v := given[key{q.id, ticket}]
		return v != "" && (q.kind != "checkbox" || v == "true")
	}
	for _, q := range qs {
		if !q.required {
			continue
		}
		for t := -1; t < len(tickets); t++ {
			if asked(q, t) && !answered(q, t) {
				return out, errMissingAnswer(q.label, q.id, t)
			}
		}
	}
	return out, nil
}

const codeAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ" // Crockford: no I, L, O or U

func newOrderCode() string {
	b := make([]byte, 5)
	_, _ = rand.Read(b) // never fails
	for i := range b {
		b[i] = codeAlphabet[b[i]%32]
	}
	return "TS-" + string(b)
}

// upsertPendingOrder writes the checkout's one order: new on the first call,
// with the latest email and details on later ones. The caller holds the
// checkout's lock, so two calls never race to insert.
func upsertPendingOrder(ctx context.Context, tx pgx.Tx, org uuid.UUID, c lockedCheckout, q pricing.Quote, d Details) (pendingOrder, error) {
	o := pendingOrder{TotalCents: q.TotalCents, FeeCents: q.FeeCents}
	details, err := json.Marshal(struct {
		Attendees []string `json:"attendees"`
		Answers   []Answer `json:"answers"`
	}{d.Attendees, d.Answers})
	if err != nil {
		return o, err
	}
	err = tx.QueryRow(ctx, `update orders set email = $2, details = $3 where checkout_id = $1 returning id, code, total_cents, fee_cents`,
		c.id, d.Email, details).Scan(&o.ID, &o.Code, &o.TotalCents, &o.FeeCents)
	switch {
	case err == nil && (o.TotalCents != q.TotalCents || o.FeeCents != q.FeeCents):
		// The catalogue's guards should make this impossible; never charge,
		// or confirm as free, an amount the buyer wasn't shown.
		return o, errPriceChanged
	case err == nil:
		var p payment
		err := tx.QueryRow(ctx, `select payment_intent_id, client_secret, stripe_account_id from payments where order_id = $1`, o.ID).
			Scan(&p.IntentID, &p.ClientSecret, &p.Account)
		if err == nil {
			o.Payment = &p
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return o, err
		}
		return o, nil
	case !errors.Is(err, pgx.ErrNoRows):
		return o, err
	}
	quote, err := json.Marshal(q)
	if err != nil {
		return o, err
	}
	var promoID *uuid.UUID
	if c.promo != nil {
		promoID = &c.promo.id
	}
	// 33 million codes: a clash is rare, and a savepoint lets us try another.
	for range 5 {
		o.Code = newOrderCode()
		sp, err := tx.Begin(ctx)
		if err != nil {
			return o, err
		}
		err = sp.QueryRow(ctx, `insert into orders (organizer_id, event_id, checkout_id, code, email, subtotal_cents, discount_cents,
				tax_cents, fee_cents, total_cents, fee_mode, promo_code_id, quote, details)
			values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14) returning id`,
			org, c.event, c.id, o.Code, d.Email, q.SubtotalCents, q.DiscountCents, q.TaxCents, q.FeeCents, q.TotalCents,
			c.feeMode, promoID, quote, details).Scan(&o.ID)
		var pg *pgconn.PgError
		if errors.As(err, &pg) && pg.Code == "23505" && pg.ConstraintName == "orders_code_key" {
			if err := sp.Rollback(ctx); err != nil {
				return o, err
			}
			continue
		}
		if err != nil {
			return o, err
		}
		return o, sp.Commit(ctx)
	}
	return o, errors.New("sales: no free order code after 5 tries")
}

// orderToken is the buyer's key to their order page: an HMAC under a key
// derived from TS_SECRET_KEY, so the email and the buyer's own browser get
// the same link without the token being stored anywhere. version 0 is the
// order id alone, as every link sent before rotation existed; each new link
// adds its version. 43 base64url characters.
func orderToken(key []byte, order uuid.UUID, version int) string {
	mac := hmac.New(sha256.New, auth.DeriveKey(key, "order-link"))
	mac.Write(order[:])
	if version > 0 {
		mac.Write(binary.BigEndian.AppendUint32(nil, uint32(version)))
	}
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// orderURL is the buyer's link to their order.
func (s *Service) orderURL(code string, order uuid.UUID, version int) string {
	return strings.TrimRight(s.StorefrontURL, "/") + "/orders/" + code + "?t=" + orderToken(s.SecretKey, order, version)
}
