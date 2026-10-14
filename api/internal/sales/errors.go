package sales

import (
	"strconv"
	"time"

	"github.com/ticketslinger/ticketslinger/api/internal/catalogue"
)

// SoldOut is 409 sold_out: what's left of every ticket type of the event.
type SoldOut struct{ Available []Availability }

func (SoldOut) Error() string { return "sold_out" }

// errTryAgain is what a buyer gets when the organizer kept rearranging
// capacity under every attempt.
var errTryAgain = catalogue.ErrConflict{Code: "try_again", Message: "The organizer is changing these tickets right now. Give it a moment and try again."}

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
)

func errQuantity(name string, min, max int) error {
	return invalid("quantity_out_of_range", "items", "You can buy between "+strconv.Itoa(min)+" and "+strconv.Itoa(max)+" "+name+" tickets in one order.")
}

// when formats t for a buyer in the event's timezone: "May 1 at 10:00".
func when(t time.Time, zone string) string {
	if loc, err := time.LoadLocation(zone); err == nil {
		t = t.In(loc)
	}
	return t.Format("January 2 at 15:04")
}

func errNotOpen(name string, start time.Time, zone string) error {
	return invalid("sales_not_open", "items", name+" goes on sale on "+when(start, zone)+".")
}

func errClosed(name string, end time.Time, zone string) error {
	return invalid("sales_closed", "items", "Sales of "+name+" closed on "+when(end, zone)+".")
}
