package sales

import (
	"errors"
	"log/slog"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/ticketslinger/ticketslinger/api/internal/catalogue"
)

// SoldOut is 409 sold_out: what's left of every ticket type of the event.
type SoldOut struct{ Available []Availability }

func (SoldOut) Error() string { return "sold_out" }

// ErrBusy means the organizer kept relinking capacity under every attempt
// (see attempts). Nothing was held. Its Error is the buyer's message; over
// HTTP it is 503 with Retry-After: 1 (mapped by the checkout handlers).
var ErrBusy error = busy{}

type busy struct{}

func (busy) Error() string { return "Tickets are moving fast — please try again." }

// ErrCheckoutExpired is 410: the hold ran out before the buyer paid.
var ErrCheckoutExpired = errors.New("sales: checkout expired")

// CheckoutCompleted is 409: this checkout already became an order.
type CheckoutCompleted struct{ OrderCode string }

func (CheckoutCompleted) Error() string { return "checkout_completed" }

// The payment provider's side. All are 503 over HTTP; nothing was held or
// written when any comes back. Unavailable may work on a retry; refused
// means the provider said no and will say it again.
var (
	ErrPaymentsNotConfigured = errors.New("sales: no payment provider")
	ErrPaymentsUnavailable   = errors.New("sales: payment provider unavailable")
	ErrPaymentsRefused       = errors.New("sales: payment provider refused")
)

// errPaymentsNotReady: the organizer's Stripe account can't take cards
// (none yet, or Stripe has charges switched off).
var errPaymentsNotReady error = catalogue.ErrConflict{Code: "payments_not_ready",
	Message: "This organizer can't take card payments right now, so paid tickets aren't on sale. Free ones still are."}

// errPriceChanged: the order's stored totals no longer match its tickets.
var errPriceChanged error = catalogue.ErrConflict{Code: "price_changed",
	Message: "The price of these tickets changed after you started paying, so we've stopped here. Start a new checkout to see the new total."}

func invalid(code, field, msg string) error {
	return catalogue.ErrInvalid{Code: code, Field: field, Message: msg}
}

var (
	errNoItems      = invalid("no_items", "items", "Pick at least one ticket.")
	errTooMany      = invalid("too_many_tickets", "items", "One order can hold up to 100 tickets. Split them across two orders.")
	errListedTwice  = invalid("too_many_tickets", "items", "List each ticket type once, with how many you want.")
	errUnknownType  = invalid("unknown_ticket_type", "items", "One of those tickets isn't on sale here. Refresh the page to see what is.")
	errEventOver    = invalid("event_over", "", "This event is over, so its tickets aren't on sale any more.")
	errPromoInvalid = invalid("promo_invalid", "promo_code", "That code isn't working for this event. Check the spelling, or it may have expired.")
	errPromoUsedUp  = invalid("promo_used_up", "promo_code", "That code has been used as many times as it allows.")
	errPromoNotHere = invalid("promo_not_applicable", "promo_code", "That code doesn't take anything off these tickets. Remove it, or pick the tickets it's for.")
	errBadEmail     = invalid("bad_email", "email", "That email address doesn't look right. Your tickets go there, so check it.")
	errBelowMinimum = invalid("below_minimum_charge", "", "Card payments start at $0.50, and this order comes to less. Add a ticket or drop the code.")
	errAttendees    = invalid("bad_attendee", "attendees", "Give at most one name per ticket, each up to 120 characters.")
)

// answerField names the answer an error is about: answers.<question id>.<ticket index>,
// with -1 for a question asked once per order.
func answerField(q uuid.UUID, ticket int) string {
	return "answers." + q.String() + "." + strconv.Itoa(ticket)
}

func errMissingAnswer(label string, q uuid.UUID, ticket int) error {
	where := ""
	if ticket >= 0 {
		where = " for ticket " + strconv.Itoa(ticket+1)
	}
	return invalid("missing_answer", answerField(q, ticket), "We need an answer to \u201c"+label+"\u201d"+where+".")
}

func errBadAnswer(q uuid.UUID, ticket int, why string) error {
	return invalid("bad_answer", answerField(q, ticket), why)
}

func errQuantity(name string, min, max int) error {
	if min == max {
		return invalid("quantity_out_of_range", "items", "You need exactly "+strconv.Itoa(min)+" "+name+" tickets in one order.")
	}
	return invalid("quantity_out_of_range", "items", "You can buy between "+strconv.Itoa(min)+" and "+strconv.Itoa(max)+" "+name+" tickets in one order.")
}

// when formats t for a buyer in the event's timezone: "Oct 14, 2026, 7:00 PM".
// The catalogue only stores zones that load; if one ever doesn't, say so in
// the log and show UTC rather than fail the sale.
func when(t time.Time, zone string) string {
	loc, err := time.LoadLocation(zone)
	if err != nil {
		slog.Warn("event timezone doesn't load; showing UTC", "zone", zone, "err", err)
		loc = time.UTC
	}
	return t.In(loc).Format("Jan 2, 2006, 3:04 PM")
}

func errNotOpen(name string, start time.Time, zone string) error {
	return invalid("sales_not_open", "items", name+" goes on sale on "+when(start, zone)+".")
}

func errClosed(name string, end time.Time, zone string) error {
	return invalid("sales_closed", "items", "Sales of "+name+" closed on "+when(end, zone)+".")
}
