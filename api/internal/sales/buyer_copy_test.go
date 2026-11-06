package sales_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/ticketslinger/ticketslinger/api/internal/mailer"
	"github.com/ticketslinger/ticketslinger/api/internal/sales"
)

// ticketEmail runs the order's confirmation job and returns the one email.
func (f fixture) ticketEmail(t *testing.T, s *sales.Service, order uuid.UUID) mailer.Message {
	t.Helper()
	payload, _ := json.Marshal(map[string]string{"order_id": order.String(), "organizer_id": f.org.String()})
	m := &mailer.Memory{}
	f.must(t, s.OrderConfirmation(m)(context.Background(), payload))
	if len(m.Sent) != 1 {
		t.Fatalf("%d emails sent, want 1", len(m.Sent))
	}
	return m.Sent[0]
}

func TestTicketEmailSaysHowTheyPaid(t *testing.T) {
	s := newService(t)
	f := seed(t, s.Pool, seedOpts{quota: 20, price: 2500})
	item := sales.Item{TicketTypeID: f.general, Quantity: 1}
	manual := func(method string) sales.ManualOrderInput {
		in := comp(f, item)
		in.Kind, in.Method, in.AmountCents = "manual", method, 4000
		return in
	}
	for name, c := range map[string]struct {
		in   sales.ManualOrderInput
		want string
	}{
		"comp":    {comp(f, item), "Complimentary."},
		"invoice": {manual("invoice"), "The organizer will invoice you ($40.00)."},
		"cash":    {manual("cash"), "Paid to the organizer ($40.00)."},
	} {
		msg := f.ticketEmail(t, s, f.manual(t, s, c.in).ID)
		if !strings.Contains(msg.Text, c.want) || !strings.Contains(msg.HTML, c.want) {
			t.Errorf("%s: no %q in\n%s", name, c.want, msg.Text)
		}
		if name == "invoice" && strings.Contains(msg.Text+msg.HTML, "Paid") {
			t.Errorf("an invoice email says Paid:\n%s", msg.Text)
		}
	}

	s2, fake := newPaidService(t)
	f2 := seed(t, s2.Pool, seedOpts{quota: 10, price: 2500})
	order, _, _ := f2.paidOrder(t, s2, fake, 1, 1)
	msg := f2.ticketEmail(t, s2, order)
	for _, no := range []string{"Complimentary", "Paid to the organizer", "invoice you"} {
		if strings.Contains(msg.Text+msg.HTML, no) {
			t.Errorf("online email mentions %q", no)
		}
	}
}
