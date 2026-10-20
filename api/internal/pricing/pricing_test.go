package pricing_test

import (
	"testing"

	"github.com/google/uuid"

	"github.com/ticketslinger/ticketslinger/api/internal/pricing"
)

var std = pricing.Fee{BP: 200, Cents: 50}

func one(price int64, taxBP int, incl, disc bool) []pricing.Line {
	return []pricing.Line{{TicketTypeID: uuid.New(), Quantity: 1, PriceCents: price, TaxRateBP: taxBP, PricesIncludeTax: incl, Discounted: disc}}
}

func TestCompute(t *testing.T) {
	pct := func(v int64) *pricing.Promo { return &pricing.Promo{Kind: "percent", Value: v} }
	amt := func(v int64) *pricing.Promo { return &pricing.Promo{Kind: "amount", Value: v} }
	cases := []struct {
		name                       string
		lines                      []pricing.Line
		promo                      *pricing.Promo
		mode                       string
		sub, disc, tax, fee, total int64
	}{
		{"$25, no tax, pass", one(2500, 0, false, false), nil, "pass", 2500, 0, 0, 100, 2600},
		{"$25, no tax, absorb", one(2500, 0, false, false), nil, "absorb", 2500, 0, 0, 100, 2500},
		{"$25 + 8.25% tax, pass", one(2500, 825, false, false), nil, "pass", 2500, 0, 206, 100, 2806},
		{"$25 + 8.25% tax, absorb", one(2500, 825, false, false), nil, "absorb", 2500, 0, 206, 100, 2706},
		// Inclusive: pre-tax 2309 (25,000,000/10,825 rounded half up), tax 191; fee on 2309 = 46 + 50.
		{"$25 incl. 8.25% tax, pass", one(2500, 825, true, false), nil, "pass", 2500, 0, 191, 96, 2596},
		{"$25 incl. 8.25% tax, absorb", one(2500, 825, true, false), nil, "absorb", 2500, 0, 191, 96, 2500},
		{"10% off $25, pass", one(2500, 0, false, true), pct(1000), "pass", 2500, 250, 0, 95, 2345},
		{"100% off is free, no fee", one(2500, 0, false, true), pct(10000), "pass", 2500, 2500, 0, 0, 0},
		{"100% off with tax is free, no tax", one(2500, 825, false, true), pct(10000), "pass", 2500, 2500, 0, 0, 0},
		{"$30 off a $25 ticket makes it free, no fee", one(2500, 0, false, true), amt(3000), "pass", 2500, 2500, 0, 0, 0},
		{"amount equal to price is free", one(2500, 0, false, true), amt(2500), "pass", 2500, 2500, 0, 0, 0},
		{"$5 off $25, pass", one(2500, 0, false, true), amt(500), "pass", 2500, 500, 0, 90, 2090},
		{"promo not for this type", one(2500, 0, false, false), pct(1000), "pass", 2500, 0, 0, 100, 2600},
		{"access code (percent 0) is full price", one(2500, 0, false, true), pct(0), "pass", 2500, 0, 0, 100, 2600},
		{"access code (amount 0) is full price", one(2500, 0, false, true), amt(0), "pass", 2500, 0, 0, 100, 2600},
		{"free ticket, no fee", one(0, 825, false, false), nil, "pass", 0, 0, 0, 0, 0},
		// 1.5 cents rounds half up to 2.
		{"fee rounding half up (75c)", one(75, 0, false, false), nil, "pass", 75, 0, 0, 52, 127},
		// 74c * 2% = 1.48 rounds down to 1.
		{"fee rounding just under half (74c)", one(74, 0, false, false), nil, "pass", 74, 0, 0, 51, 125},
		// 25c * 10% = 2.5 rounds up to 3, leaving 22.
		{"discount rounding half up", one(25, 0, false, true), pct(1000), "pass", 25, 3, 0, 50, 72},
		// 24c * 10% = 2.4 rounds down to 2.
		{"discount rounding just under half", one(24, 0, false, true), pct(1000), "pass", 24, 2, 0, 50, 72},
		// 50c * 8.25% = 4.125 -> 4; 60c * 8.25% = 4.95 -> 5; 6c * 825 = 0.495 -> 0; 7c -> 0.5775 -> 1.
		{"tax rounds down (50c)", one(50, 825, false, false), nil, "pass", 50, 0, 4, 51, 105},
		{"tax rounds up (60c)", one(60, 825, false, false), nil, "pass", 60, 0, 5, 51, 116},
		{"tax half up at exactly .5 (10c at 5%)", one(10, 500, false, false), nil, "pass", 10, 0, 1, 50, 61},
		{"two tickets", []pricing.Line{{Quantity: 2, PriceCents: 1000}}, nil, "pass", 2000, 0, 0, 140, 2140},
		{"empty order", nil, nil, "pass", 0, 0, 0, 0, 0},
	}
	for _, c := range cases {
		q := pricing.Compute(c.lines, c.promo, c.mode, std)
		if q.SubtotalCents != c.sub || q.DiscountCents != c.disc || q.TaxCents != c.tax || q.FeeCents != c.fee || q.TotalCents != c.total {
			t.Errorf("%s: got sub %d disc %d tax %d fee %d total %d; want %d %d %d %d %d", c.name,
				q.SubtotalCents, q.DiscountCents, q.TaxCents, q.FeeCents, q.TotalCents, c.sub, c.disc, c.tax, c.fee, c.total)
		}
		if q.FeeMode != c.mode {
			t.Errorf("%s: fee mode %q", c.name, q.FeeMode)
		}
	}
}

// Stripe refuses an application fee larger than the charge. Absorbing a fee
// on a 40-cent ticket would be exactly that.
func TestAbsorbFeeNeverExceedsCharge(t *testing.T) {
	q := pricing.Compute(one(40, 0, false, false), nil, "absorb", std)
	if q.FeeCents != 40 || q.TotalCents != 40 {
		t.Fatalf("fee %d total %d; want the fee capped at the 40-cent charge", q.FeeCents, q.TotalCents)
	}
	if q.TotalCents >= pricing.MinimumChargeCents {
		t.Fatal("40 cents is under Stripe's minimum; checkout must refuse it")
	}
}

func TestTicketsSumToTotals(t *testing.T) {
	lines := []pricing.Line{{Quantity: 3, PriceCents: 1999, TaxRateBP: 725, Discounted: true}, {Quantity: 2, PriceCents: 4500, TaxRateBP: 725, PricesIncludeTax: true}}
	q := pricing.Compute(lines, &pricing.Promo{Kind: "percent", Value: 1500}, "pass", std)
	var charge, fee, disc, tax int64
	for _, tk := range q.Tickets {
		charge += tk.ChargeCents
		fee += tk.FeeCents
		disc += tk.DiscountCents
		tax += tk.TaxCents
	}
	if len(q.Tickets) != 5 || charge+fee != q.TotalCents || fee != q.FeeCents || disc != q.DiscountCents || tax != q.TaxCents {
		t.Fatalf("tickets don't add up: %d tickets, charges %d + fees %d vs total %d", len(q.Tickets), charge, fee, q.TotalCents)
	}
}

// Anything that isn't absorb behaves exactly as pass: fee uncapped and added to the total.
func TestUnknownFeeModeIsPass(t *testing.T) {
	for _, l := range [][]pricing.Line{one(2500, 825, false, false), one(40, 0, false, false)} {
		want := pricing.Compute(l, nil, pricing.FeePass, std)
		got := pricing.Compute(l, nil, "", std)
		got.FeeMode = want.FeeMode
		if got.SubtotalCents != want.SubtotalCents || got.FeeCents != want.FeeCents || got.TotalCents != want.TotalCents || got.TaxCents != want.TaxCents {
			t.Fatalf("empty mode %+v; want same as pass %+v", got, want)
		}
	}
	if q := pricing.Compute(one(40, 0, false, false), nil, pricing.FeeAbsorb, std); q.FeeCents != 40 {
		t.Fatalf("absorb fee %d", q.FeeCents)
	}
}

func TestQuoteSaysWhichTaxIsIncluded(t *testing.T) {
	tt := uuid.New()
	in := pricing.Compute([]pricing.Line{{TicketTypeID: tt, Quantity: 2, PriceCents: 2200, TaxRateBP: 1000, PricesIncludeTax: true}}, nil, pricing.FeePass, pricing.Fee{})
	ex := pricing.Compute([]pricing.Line{{TicketTypeID: tt, Quantity: 1, PriceCents: 2000, TaxRateBP: 1000}}, nil, pricing.FeePass, pricing.Fee{})
	if !in.Tickets[0].TaxIncluded || in.IncludedTaxCents != 400 || in.TaxCents != 400 {
		t.Fatalf("inclusive: %+v", in)
	}
	if ex.Tickets[0].TaxIncluded || ex.IncludedTaxCents != 0 || ex.Tickets[0].ChargeCents != 2200 {
		t.Fatalf("exclusive: %+v", ex)
	}
	// 0% inclusive tax is still "included", even though charge == net.
	zero := pricing.Compute([]pricing.Line{{TicketTypeID: tt, Quantity: 1, PriceCents: 1000, PricesIncludeTax: true}}, nil, pricing.FeePass, pricing.Fee{})
	if !zero.Tickets[0].TaxIncluded {
		t.Fatal("0% inclusive tax lost its flag")
	}
}

type allInCase struct {
	price int64
	tax   int
	incl  bool
	mode  string
	fee   pricing.Fee
}

func allInGrid() []allInCase {
	var g []allInCase
	for _, p := range []int64{0, 1, 49, 50, 99, 2500, 2165, 99999, 10000000} {
		for _, tx := range []struct {
			bp   int
			incl bool
		}{{0, false}, {825, false}, {825, true}, {1000, false}, {1000, true}} {
			for _, m := range []string{pricing.FeePass, pricing.FeeAbsorb} {
				for _, f := range []pricing.Fee{{BP: 200, Cents: 50}, {}, {BP: 1000}, {Cents: 99}} {
					g = append(g, allInCase{p, tx.bp, tx.incl, m, f})
				}
			}
		}
	}
	return g
}

func TestShownPriceMatchesCompute(t *testing.T) {
	for _, c := range allInGrid() {
		l := pricing.Line{TicketTypeID: uuid.New(), Quantity: 1, PriceCents: c.price, TaxRateBP: c.tax, PricesIncludeTax: c.incl}
		s := pricing.ShownPrice(l, c.mode, c.fee)
		q := pricing.Compute([]pricing.Line{l}, nil, c.mode, c.fee)
		wantFee := q.FeeCents
		if c.mode == pricing.FeeAbsorb {
			wantFee = 0
		}
		if s.AllInCents != q.TotalCents || s.FeeCents != wantFee || s.TaxCents != q.TaxCents {
			t.Fatalf("%+v: shown %+v, quote %+v", c, s, q)
		}
		if q.Tickets[0].AllInCents != q.TotalCents {
			t.Fatalf("%+v: ticket all-in %d, total %d", c, q.Tickets[0].AllInCents, q.TotalCents)
		}
	}
}

func TestAllInDiscountIsHonest(t *testing.T) {
	promos := []*pricing.Promo{
		nil,
		{Kind: "percent", Value: 0},
		{Kind: "percent", Value: 1000},
		{Kind: "percent", Value: 3333},
		{Kind: "percent", Value: 10000},
		{Kind: "amount", Value: 100},
		{Kind: "amount", Value: 500},
	}
	for _, c := range allInGrid() {
		ps := append([]*pricing.Promo{}, promos...)
		ps = append(ps, &pricing.Promo{Kind: "amount", Value: c.price + 1})
		for _, p := range ps {
			for n := 1; n <= 3; n++ {
				l := pricing.Line{TicketTypeID: uuid.New(), Quantity: n, PriceCents: c.price, TaxRateBP: c.tax, PricesIncludeTax: c.incl, Discounted: true}
				q := pricing.Compute([]pricing.Line{l}, p, c.mode, c.fee)
				var sum int64
				for _, tk := range q.Tickets {
					sum += tk.AllInCents
				}
				if sum-q.AllInDiscountCents != q.TotalCents || q.AllInDiscountCents < 0 {
					t.Fatalf("%+v promo %+v n=%d: %+v", c, p, n, q)
				}
				if (p == nil || p.Value == 0) && q.AllInDiscountCents != 0 {
					t.Fatalf("%+v promo %+v n=%d: discount %d, want 0", c, p, n, q.AllInDiscountCents)
				}
			}
		}
	}
}

func TestWorkedExamples(t *testing.T) {
	fee := pricing.Fee{BP: 200, Cents: 50}
	cases := []struct {
		name                string
		l                   pricing.Line
		promo               *pricing.Promo
		mode                string
		allIn, feeC, taxC   int64
		total, allInDiscont int64
	}{
		{"general pass", pricing.Line{Quantity: 1, PriceCents: 2500}, nil, pricing.FeePass, 2600, 100, 0, 2600, 0},
		{"two with FRIENDS", pricing.Line{Quantity: 2, PriceCents: 2500, Discounted: true}, &pricing.Promo{Kind: "percent", Value: 1000}, pricing.FeePass, 2600, 100, 0, 4690, 510},
		{"tax on top", pricing.Line{Quantity: 1, PriceCents: 2000, TaxRateBP: 825}, nil, pricing.FeePass, 2255, 90, 165, 2255, 0},
		{"tax included", pricing.Line{Quantity: 1, PriceCents: 2165, TaxRateBP: 825, PricesIncludeTax: true}, nil, pricing.FeePass, 2255, 90, 165, 2255, 0},
		{"absorb", pricing.Line{Quantity: 1, PriceCents: 2500}, nil, pricing.FeeAbsorb, 2500, 0, 0, 2500, 0},
		{"free", pricing.Line{Quantity: 1, PriceCents: 0}, nil, pricing.FeePass, 0, 0, 0, 0, 0},
		{"direct mode", pricing.Line{Quantity: 1, PriceCents: 2500}, nil, pricing.FeePass, 2500, 0, 0, 2500, 0},
	}
	for _, c := range cases {
		f := fee
		if c.name == "direct mode" {
			f = pricing.Fee{}
		}
		s := pricing.ShownPrice(c.l, c.mode, f)
		if s.AllInCents != c.allIn || s.FeeCents != c.feeC || s.TaxCents != c.taxC {
			t.Errorf("%s: shown %+v", c.name, s)
		}
		q := pricing.Compute([]pricing.Line{c.l}, c.promo, c.mode, f)
		if q.TotalCents != c.total || q.AllInDiscountCents != c.allInDiscont {
			t.Errorf("%s: total %d discount %d", c.name, q.TotalCents, q.AllInDiscountCents)
		}
	}
}
