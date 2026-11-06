package sales

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ticketslinger/ticketslinger/api/internal/audit"
	"github.com/ticketslinger/ticketslinger/api/internal/catalogue"
	"github.com/ticketslinger/ticketslinger/api/internal/pricing"
	"github.com/ticketslinger/ticketslinger/api/internal/system"
)

// ManualOrderInput is an owner or admin's order: comp (free) or manual
// (paid outside Stripe, AmountCents as collected, Method how).
type ManualOrderInput struct {
	Event                  uuid.UUID
	Kind, Method           string // comp|manual; cash|cheque|invoice|other
	AmountCents            int64
	Email, BuyerName, Note string
	Items                  []Item
	Attendees              []string
	SendTickets            bool
}

type ManualOrderResult struct {
	ID   uuid.UUID
	Code string
}

// ErrNotManager is 403 forbidden_role: the member isn't an owner or admin.
var ErrNotManager = errors.New("sales: only an owner or admin can do that")

// The API's bound on a manual amount: $10M, which keeps the split in int64.
const maxManualCents = 1_000_000_000

var manualMethods = []string{"cash", "cheque", "invoice", "other"}

// CreateManualOrder issues tickets for an owner or admin (member) of org,
// paid at once, with no checkout, payment or fee. Any ticket type of the
// event may be issued (hidden, outside its sales window, on a draft or ended
// event), but quotas hold: the quota locks are the ones confirmOrder takes,
// so it can't oversell against a checkout. Promo codes and questions don't
// apply. It runs as ts_system, so the member's role is checked first, in the
// same transaction.
func (s *Service) CreateManualOrder(ctx context.Context, org, member uuid.UUID, in ManualOrderInput) (ManualOrderResult, error) {
	in, err := checkManual(in)
	if err != nil {
		return ManualOrderResult{}, err
	}
	var out ManualOrderResult
	err = system.Run(ctx, s.Pool, org, func(tx pgx.Tx) error {
		// The event first, held FOR SHARE, as Pay does: a cancellation waits
		// for this order, then finds it.
		var status, feeMode string
		err := tx.QueryRow(ctx, `select status, fee_mode from events where id = $1 for share`, in.Event).Scan(&status, &feeMode)
		if errors.Is(err, pgx.ErrNoRows) {
			return catalogue.ErrNotFound
		}
		if err != nil {
			return err
		}
		var manager bool
		if err := tx.QueryRow(ctx, `select order_manager($1)`, member).Scan(&manager); err != nil {
			return err
		}
		if !manager {
			return ErrNotManager
		}
		if err := offSale(ctx, tx, org); err != nil {
			return err
		}
		if status == "cancelled" {
			return errOrderEventCancelled
		}
		ids := itemIDs(in.Items)
		// The quota locks, then everything decided on is read after them.
		sizes, links, err := lockQuotas(ctx, tx, in.Event, ids)
		if errors.Is(err, errQuotasMoved) {
			return ErrBusy
		}
		if err != nil {
			return err
		}
		tts, err := ticketTypes(ctx, tx, in.Event, ids, nil)
		if err != nil {
			return err
		}
		for _, it := range in.Items {
			if _, ok := tts[it.TicketTypeID]; !ok {
				return errManualType
			}
		}
		for _, it := range in.Items {
			if len(links[it.TicketTypeID]) == 0 {
				return errNoQuota(tts[it.TicketTypeID].name)
			}
		}
		// fits names only what a buyer sees; the organizer sees every type.
		// The second availability read only runs on a refusal.
		var so SoldOut
		if err := fits(ctx, tx, in.Event, sizes, need(in.Items, links), uuid.Nil, nil); errors.As(err, &so) {
			return soldOutAll(ctx, tx, in.Event)
		} else if err != nil {
			return err
		}
		ls := lines(tts, in.Items, nil)
		q := pricing.Comp(ls, feeMode)
		var method *string
		if in.Kind == "manual" {
			q, method = pricing.Manual(ls, in.AmountCents, feeMode), &in.Method
		}
		d := Details{Attendees: in.Attendees, Answers: []Answer{}}
		details, err := json.Marshal(struct {
			Attendees []string `json:"attendees"`
			Answers   []Answer `json:"answers"`
		}{d.Attendees, d.Answers})
		if err != nil {
			return err
		}
		out.ID, out.Code, err = insertOrder(ctx, tx, newOrder{Org: org, Event: in.Event, Email: in.Email, Quote: q, FeeMode: feeMode,
			Details: details, Source: in.Kind, ManualMethod: method, CreatedBy: &member, BuyerName: in.BuyerName})
		if err != nil {
			return err
		}
		if err := cut(ctx, tx, org, in.Event, out.ID, q, d); err != nil {
			return err
		}
		if err := s.settle(ctx, tx, out.ID, "paid"); err != nil {
			return err
		}
		// No email, buyer or attendee name: the order's id is enough.
		if err := audit.Write(ctx, tx, audit.Entry{Organizer: org, Actor: audit.Actor{User: member}, Action: "order.created_manual",
			EntityType: "order", EntityID: out.ID, After: map[string]any{"kind": in.Kind, "method": method, "amount_cents": q.TotalCents,
				"lines": in.Items, "send_tickets": in.SendTickets}}); err != nil {
			return err
		}
		if in.Note != "" {
			// ts_system can't read order_notes back, so the id is made here.
			note, err := uuid.NewV7()
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `insert into order_notes (id, organizer_id, order_id, author, body) values ($1, $2, $3, $4, $5)`,
				note, org, out.ID, member, in.Note); err != nil {
				return err
			}
			if err := audit.Write(ctx, tx, audit.Entry{Organizer: org, Actor: audit.Actor{User: member}, Action: "order.note_added",
				EntityType: "order", EntityID: out.ID, After: map[string]string{"note_id": note.String()}}); err != nil {
				return err
			}
		}
		if in.SendTickets {
			return queueTicketEmail(ctx, tx, out.ID)
		}
		return nil
	})
	return out, err
}

// checkManual checks in before any query and returns it cleaned.
func checkManual(in ManualOrderInput) (ManualOrderInput, error) {
	switch in.Kind {
	case "comp", "manual":
	case "":
		return in, errNoKind
	default:
		return in, errBadKind
	}
	if err := checkItems(in.Items); err != nil {
		var inv catalogue.ErrInvalid
		if errors.As(err, &inv) {
			inv.Field = "lines" // the body's name for them
			return in, inv
		}
		return in, err
	}
	if (in.Kind == "manual") != slices.Contains(manualMethods, in.Method) {
		return in, errBadMethod
	}
	if in.AmountCents < 0 || in.AmountCents > maxManualCents || in.Kind == "comp" && in.AmountCents != 0 {
		return in, errBadAmount
	}
	if in.Email = strings.TrimSpace(in.Email); !goodEmail(in.Email) {
		return in, errOrderEmail
	}
	var err error
	if in.BuyerName, err = CleanName(in.BuyerName); err != nil {
		return in, errBuyerName
	}
	tickets := 0
	for _, it := range in.Items {
		tickets += it.Quantity
	}
	if len(in.Attendees) > tickets {
		return in, errTooManyNames
	}
	names := make([]string, len(in.Attendees))
	for i, a := range in.Attendees {
		if names[i], err = CleanName(a); err != nil {
			return in, err
		}
	}
	in.Attendees = names
	var ok bool
	if in.Note, ok = cleanText(in.Note, 2000); !ok {
		return in, errBadNote
	}
	return in, nil
}
