package sales_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ticketslinger/ticketslinger/api/internal/sales"
	"github.com/ticketslinger/ticketslinger/api/internal/system"
)

// One roll-up for refund and cancel alike: a void ticket only reads as
// refunded when a refund took it.
func TestOrderStatusRollUp(t *testing.T) {
	s, fake := newPaidService(t)
	f := seed(t, s.Pool, seedOpts{quota: 4, price: 2500})
	ctx := context.Background()
	order, _, tickets := f.paidOrder(t, s, fake, 2, 1)
	payment := f.id(t, `select id from payments where order_id = $1`, order)
	var refund uuid.UUID
	f.must(t, system.Run(ctx, s.Pool, f.org, func(tx pgx.Tx) (err error) {
		refund, err = sales.RequestRefundForTest(ctx, tx, f.org, payment, 2500, 0, true, "organizer", nil)
		return err
	}))
	// state: "v" valid, "c" void by cancel, "r" void by refund.
	cases := []struct{ name, a, b, want string }{
		{"none void", "v", "v", "paid"},
		{"one void, no refund", "c", "v", "paid"},
		{"one void, refunded", "r", "v", "partially_refunded"},
		{"both void, none refunded", "c", "c", "cancelled"},
		{"both void, one refunded", "r", "c", "refunded"},
		{"free order, both released", "c", "c", "cancelled"},
		{"paid order, both refunded", "r", "r", "refunded"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			for i, st := range []string{c.a, c.b} {
				switch st {
				case "v":
					f.exec(t, `update tickets set status = 'valid', refund_id = null, released_at = null where id = $1`, tickets[i])
				case "c":
					f.exec(t, `update tickets set status = 'void', refund_id = null, released_at = now() where id = $1`, tickets[i])
				case "r":
					f.exec(t, `update tickets set status = 'void', refund_id = $2, released_at = null where id = $1`, tickets[i], refund)
				}
			}
			var got string
			f.must(t, system.Run(ctx, s.Pool, f.org, func(tx pgx.Tx) (err error) {
				got, err = sales.RollUpOrderForTest(ctx, tx, order)
				return err
			}))
			if got != c.want {
				t.Fatalf("got %q, want %q", got, c.want)
			}
		})
	}
}
