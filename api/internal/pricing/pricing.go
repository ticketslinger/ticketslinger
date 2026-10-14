// Package pricing works out what a buyer pays and what the platform keeps.
// All amounts are integer cents; every rounding is half up, per ticket.
package pricing

import "github.com/google/uuid"

// MinimumChargeCents is Stripe's USD minimum for a charge.
const MinimumChargeCents = 50

// Fee modes. Anything other than FeeAbsorb is priced as FeePass.
const (
	FeePass   = "pass"
	FeeAbsorb = "absorb"
)

// Fee is the platform fee, charged per paid ticket.
type Fee struct {
	BP    int   // basis points of the ticket's pre-tax price
	Cents int64 // flat
}

// Line is one ticket type in an order.
type Line struct {
	TicketTypeID     uuid.UUID
	Quantity         int
	PriceCents       int64
	TaxRateBP        int  // already resolved: the ticket type's override or the event's
	PricesIncludeTax bool // likewise
	Discounted       bool // the promo applies to this ticket type
}

// Promo is a discount code. Value 0 is an access code: it unlocks, never discounts.
type Promo struct {
	Kind  string // "percent" (Value in bp) or "amount" (Value in cents, per ticket)
	Value int64
}

// Ticket is the price of one ticket.
type Ticket struct {
	TicketTypeID                                  uuid.UUID
	PriceCents, DiscountCents, TaxCents, FeeCents int64
	ChargeCents                                   int64 // what the buyer pays for this ticket, excluding a passed fee
}

// Quote is the price of a whole order.
type Quote struct {
	Tickets                                                      []Ticket
	SubtotalCents, DiscountCents, TaxCents, FeeCents, TotalCents int64
	FeeMode                                                      string // "pass" | "absorb"
	// FeeCents is the platform's application fee either way; TotalCents
	// includes it only when FeeMode is "pass".
}

// bp is amount × rate / 10000, rounded half up. amount and rate are >= 0.
func bp(amount, rate int64) int64 { return (amount*rate + 5000) / 10000 }

// Compute prices every ticket in lines. Free tickets (including ones a promo
// makes free) carry no fee. In absorb mode a ticket's fee is capped at its charge;
// any other feeMode is treated as pass.
//
// Inputs must be within the database CHECK bounds: non-negative prices and
// values, tax 0-10000 bp, percent promo at most 10000. Callers must not pass
// unvalidated request data.
func Compute(lines []Line, promo *Promo, feeMode string, fee Fee) Quote {
	q := Quote{FeeMode: feeMode}
	for _, l := range lines {
		for i := 0; i < l.Quantity; i++ {
			t := Ticket{TicketTypeID: l.TicketTypeID, PriceCents: l.PriceCents}
			if promo != nil && l.Discounted {
				switch promo.Kind {
				case "percent":
					t.DiscountCents = min(bp(l.PriceCents, promo.Value), l.PriceCents)
				case "amount":
					t.DiscountCents = min(promo.Value, l.PriceCents)
				}
			}
			net := l.PriceCents - t.DiscountCents
			preTax := net
			if l.PricesIncludeTax {
				d := int64(10000 + l.TaxRateBP)
				preTax = (net*10000 + d/2) / d
				t.TaxCents = net - preTax
				t.ChargeCents = net
			} else {
				t.TaxCents = bp(net, int64(l.TaxRateBP))
				t.ChargeCents = net + t.TaxCents
			}
			if net > 0 {
				t.FeeCents = bp(preTax, int64(fee.BP)) + fee.Cents
				if feeMode == FeeAbsorb {
					t.FeeCents = min(t.FeeCents, t.ChargeCents)
				}
			}
			q.Tickets = append(q.Tickets, t)
			q.SubtotalCents += t.PriceCents
			q.DiscountCents += t.DiscountCents
			q.TaxCents += t.TaxCents
			q.FeeCents += t.FeeCents
			q.TotalCents += t.ChargeCents
		}
	}
	if feeMode != FeeAbsorb {
		q.TotalCents += q.FeeCents
	}
	return q
}
