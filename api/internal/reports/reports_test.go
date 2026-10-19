package reports_test

import (
	"bytes"
	"context"
	"encoding/csv"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ticketslinger/ticketslinger/api/internal/catalogue"
	"github.com/ticketslinger/ticketslinger/api/internal/db"
	"github.com/ticketslinger/ticketslinger/api/internal/reports"
)

func TestReportFigures(t *testing.T) {
	f := seed(t)
	f.sales(t)
	r := f.report(t)
	// Over the order list's orders (A, B, C and the late payment L; not the
	// abandoned X): gross 4,600 + 5,500 + 2,500 + 2,600; refunded B's 2,500,
	// C's requested 1,000 and L's 2,600 (C's failed 100 isn't money back); our
	// fee 4 × 100 less what has gone back, B's 50 and L's 100 (C's 30 is still
	// to come); B's tax; four orders.
	if r.GrossCents != 15200 || r.RefundedCents != 6100 || r.PlatformFeeCents != 250 || r.TaxCents != 400 || r.NetCents != 8850 {
		t.Fatalf("money: %+v", r)
	}
	if r.Orders != 4 || r.Sold != 4 || r.CheckedIn != 1 {
		t.Fatalf("counts: orders %d sold %d checked in %d", r.Orders, r.Sold, r.CheckedIn)
	}
	wantTypes := []reports.TypeLine{{ID: f.general, Name: "General", Sold: 3, CheckedIn: 1}, {ID: f.vip, Name: "VIP", Sold: 1}}
	if !reflect.DeepEqual(r.TicketTypes, wantTypes) {
		t.Fatalf("ticket types: %+v", r.TicketTypes)
	}
	if len(r.Quotas) != 1 || r.Quotas[0].Name != "Hall" || r.Quotas[0].Size != 10 || r.Quotas[0].Remaining != 10-r.Sold {
		t.Fatalf("quotas: %+v", r.Quotas)
	}
	// Denver dates, not the UTC ones (1 and 2 March); L counts on the day it was placed.
	wantDays := []reports.DayLine{{Day: "2026-02-28", Orders: 2, Tickets: 3, GrossCents: 10100}, {Day: "2026-03-01", Orders: 2, Tickets: 1, GrossCents: 5100}}
	if !reflect.DeepEqual(r.SalesByDay, wantDays) {
		t.Fatalf("sales by day: %+v", r.SalesByDay)
	}
	if len(r.PromoCodes) != 1 || r.PromoCodes[0] != (reports.PromoLine{Code: "EARLY", UsedCount: 1, DiscountCents: 500}) {
		t.Fatalf("promo codes: %+v", r.PromoCodes)
	}
}

func TestEmptyEventReport(t *testing.T) {
	f := seed(t)
	r := f.report(t)
	if r.GrossCents != 0 || r.RefundedCents != 0 || r.PlatformFeeCents != 0 || r.TaxCents != 0 || r.NetCents != 0 || r.Orders != 0 || r.Sold != 0 || r.CheckedIn != 0 {
		t.Fatalf("figures: %+v", r)
	}
	if r.SalesByDay == nil || len(r.SalesByDay) != 0 || r.TicketTypes == nil || r.Quotas == nil || r.PromoCodes == nil {
		t.Fatalf("slices: %+v", r)
	}
	if r.Quotas[0].Remaining != 10 || r.PromoCodes[0].DiscountCents != 0 || r.TicketTypes[0].Sold != 0 {
		t.Fatalf("lines: %+v", r)
	}
}

func TestUnknownEvent(t *testing.T) {
	f := seed(t)
	ctx := context.Background()
	err := db.WithOrganizer(ctx, f.app, f.user, f.org, func(tx pgx.Tx, _ string) error {
		_, err := reports.Event(ctx, tx, uuid.New())
		if !errors.Is(err, catalogue.ErrNotFound) {
			t.Errorf("report: %v, want ErrNotFound", err)
		}
		_, err = reports.WriteCSV(ctx, tx, uuid.New(), "orders.csv", &bytes.Buffer{})
		if !errors.Is(err, catalogue.ErrNotFound) {
			t.Errorf("csv: %v, want ErrNotFound", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// rows parses an export: BOM first, CRLF line ends.
func rows(t *testing.T, b []byte) [][]string {
	t.Helper()
	if !bytes.HasPrefix(b, []byte("\xEF\xBB\xBF")) {
		t.Fatalf("no BOM: %q", b[:min(len(b), 10)])
	}
	if !bytes.HasSuffix(b, []byte("\r\n")) || bytes.Count(b, []byte("\n")) != bytes.Count(b, []byte("\r\n")) {
		t.Fatalf("not CRLF: %q", b)
	}
	out, err := csv.NewReader(bytes.NewReader(b[3:])).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestCSVColumns(t *testing.T) {
	f := seed(t)
	_, ticketsA := f.sales(t)
	cases := map[string]string{
		"orders.csv":    "Order,Status,Email,Tickets bought,Subtotal,Discount,Tax,Service fee,Fee paid by,Total,Refunded,Promo code,Placed,Paid",
		"attendees.csv": "Order,Ticket,Ticket type,Attendee,Email,Status,Checked in,Dietary needs,T-shirt",
		"checkins.csv":  "Time,Result,Method,Ticket,Ticket type,Attendee,Order,Gate,By",
	}
	got := map[string][][]string{}
	for kind, header := range cases {
		name, b := f.csv(t, kind)
		if name != "gala-"+kind {
			t.Errorf("%s: filename %q", kind, name)
		}
		got[kind] = rows(t, b)
		if h := strings.Join(got[kind][0], ","); h != header {
			t.Errorf("%s header: %s", kind, h)
		}
	}
	// Orders, newest first: L, C, B, A. Money as dollars, times in Denver.
	orders := got["orders.csv"]
	if len(orders) != 5 {
		t.Fatalf("orders: %q", orders)
	}
	wantA := []string{"TS-AAAAA", "paid", "a@example.com", "2", "50.00", "5.00", "0.00", "1.00", "buyer", "46.00", "0.00", "EARLY", "2026-02-28T18:28:00-07:00", "2026-02-28T18:30:00-07:00"}
	if !reflect.DeepEqual(orders[4], wantA) {
		t.Errorf("order A: %q", orders[4])
	}
	if b := orders[3]; b[0] != "TS-BBBBB" || b[10] != "25.00" || b[11] != "" {
		t.Errorf("order B: %q", b)
	}
	if c := orders[2]; c[0] != "TS-CCCCC" || c[8] != "you" || c[9] != "25.00" || c[10] != "10.00" {
		t.Errorf("order C: %q", c)
	}
	if l := orders[1]; l[0] != "TS-PPPPP" || l[1] != "cancelled" || l[3] != "0" || l[10] != "26.00" || l[13] != "" {
		t.Errorf("order L: %q", l)
	}
	// Attendees: the order-scope answer is on both of A's tickets, the
	// ticket-scope one only on its own.
	att := map[string][]string{}
	for _, r := range got["attendees.csv"][1:] {
		att[r[1]] = r
	}
	a0, a1 := att[ticketsA[0].String()], att[ticketsA[1].String()]
	if a0 == nil || a0[0] != "TS-AAAAA" || a0[2] != "General" || a0[3] != "Ada" || a0[4] != "a@example.com" || a0[5] != "valid" ||
		a0[6] != "2026-03-04T19:10:00-07:00" || a0[7] != "Vegan" || a0[8] != "M" {
		t.Errorf("Ada: %q", a0)
	}
	if a1 == nil || a1[3] != "Bea" || a1[6] != "" || a1[7] != "Vegan" || a1[8] != "" {
		t.Errorf("Bea: %q", a1)
	}
	if len(att) != 4 {
		t.Errorf("attendees: %d rows", len(att))
	}
	want := [][]string{
		{"2026-03-04T19:10:00-07:00", "accepted", "scan", ticketsA[0].String(), "General", "Ada", "TS-AAAAA", "North", "owner@example.com"},
		{"2026-03-04T19:11:00-07:00", "duplicate", "scan", ticketsA[0].String(), "General", "Ada", "TS-AAAAA", "", "API key abcd1234"},
	}
	if !reflect.DeepEqual(got["checkins.csv"][1:], want) {
		t.Errorf("checkins: %q", got["checkins.csv"][1:])
	}
}

func TestCSVDefusesFormulas(t *testing.T) {
	f := seed(t)
	o, ts := f.order(t, sale{code: "TS-DDDDD", email: "@x.test", placed: "2026-03-01T01:28:00Z", paid: true,
		subtotal: 2500, fee: 100, total: 2600, ticketType: f.general, names: []string{`=HYPERLINK("x")`, "-"}})
	f.answer(t, o, nil, f.dietary, "+SUM(A1)")
	f.answer(t, o, &ts[0], f.tshirt, "\tL")
	_, b := f.csv(t, "attendees.csv")
	att := map[string][]string{}
	for _, r := range rows(t, b)[1:] {
		att[r[1]] = r
	}
	first, second := att[ts[0].String()], att[ts[1].String()]
	if first[3] != `'=HYPERLINK("x")` || first[4] != "'@x.test" || first[7] != "'+SUM(A1)" || first[8] != "'\tL" {
		t.Errorf("first: %q", first)
	}
	if second[3] != "'-" {
		t.Errorf("second: %q", second)
	}
	_, b = f.csv(t, "orders.csv")
	if r := rows(t, b)[1]; r[2] != "'@x.test" {
		t.Errorf("orders: %q", r)
	}
	if bytes.Contains(b, []byte("\r\n=")) || bytes.Contains(b, []byte(",=")) {
		t.Errorf("a cell starts with =: %q", b)
	}
}

func TestUnknownKind(t *testing.T) {
	f := seed(t)
	ctx := context.Background()
	_ = db.WithOrganizer(ctx, f.app, f.user, f.org, func(tx pgx.Tx, _ string) error {
		if _, err := reports.WriteCSV(ctx, tx, f.event, "secrets.csv", &bytes.Buffer{}); !errors.Is(err, reports.ErrUnknownKind) {
			t.Errorf("got %v, want ErrUnknownKind", err)
		}
		return nil
	})
}

// An API key isn't handed members' emails: a person's check-in reads "Team
// member" in its export.
func TestCheckinsByForKeys(t *testing.T) {
	f := seed(t)
	f.sales(t)
	key := f.id(t, `insert into api_keys(organizer_id, name, key_hash, prefix, scope) values ($1, 'Reader', '\x02', 'read0000', 'read') returning id`, f.org)
	ctx := context.Background()
	var buf bytes.Buffer
	err := db.WithPrincipal(ctx, f.app, db.Principal{KeyID: key, KeyOrg: f.org, KeyScope: "read"}, f.org, func(tx pgx.Tx, _ string) error {
		_, err := reports.WriteCSV(ctx, tx, f.event, "checkins.csv", &buf)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	got := rows(t, buf.Bytes())
	if got[1][8] != "Team member" || got[2][8] != "API key abcd1234" || strings.Contains(buf.String(), "owner@example.com") {
		t.Fatalf("by: %q", got)
	}
}
