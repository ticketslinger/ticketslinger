package httpapi

import (
	"context"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ticketslinger/ticketslinger/api/internal/catalogue"
	"github.com/ticketslinger/ticketslinger/api/internal/checkin"
	"github.com/ticketslinger/ticketslinger/api/internal/organizers"
)

var (
	checkinWrite = Access{Roles: []string{"owner", "admin", "checkin"}, Write: true}

	errQROrTicket   = &apiError{422, "qr_or_ticket", "Send either qr or ticket_id."}
	errInvalidQR    = invalidField("invalid_qr", "qr", "Send the text the scanner read: 1 to 200 characters.")
	errInvalidGate  = invalidField("invalid_gate", "gate", "Keep the gate's name to 40 characters.")
	errInvalidQuery = invalidField("invalid_q", "q", "Search with 2 to 120 characters: a name, an email or an order code.")
	errTooManyScans = retryError{&apiError{429, "rate_limited", "That's a lot of scans in one minute. Give it a moment, then scan again."}, "60"}
)

// inEvent runs fn in the caller's organizer once RLS has found event there,
// so another tenant's event, or none, is a 404 before anything is written.
func (s *server) inEvent(ctx context.Context, slug string, event uuid.UUID, access Access, fn func(org organizers.Organizer, tx pgx.Tx) error) error {
	return catalogueError(s.inOrganizer(ctx, slug, access, func(org organizers.Organizer, tx pgx.Tx) error {
		if _, err := catalogue.GetEvent(ctx, tx, event); err != nil {
			return err
		}
		return fn(org, tx)
	}))
}

// CreateCheckin never logs the scanned text: it is the ticket's credential.
func (s *server) CreateCheckin(ctx context.Context, req CreateCheckinRequestObject) (CreateCheckinResponseObject, error) {
	var out CreateCheckin200JSONResponse
	err := s.inEvent(ctx, req.Organizer, req.Event, checkinWrite, func(org organizers.Organizer, tx pgx.Tx) error {
		b := req.Body
		if b.Qr != nil && (*b.Qr == "" || utf8.RuneCountInString(*b.Qr) > 200) {
			return errInvalidQR
		}
		if b.Gate != nil && utf8.RuneCountInString(*b.Gate) > 40 {
			return errInvalidGate
		}
		if (b.Qr == nil) == (b.TicketId == nil) {
			return errQROrTicket
		}
		in := checkin.Input{Actor: actorFrom(ctx)}
		if b.Qr != nil {
			in.QR = *b.Qr
		} else {
			in.TicketID = *b.TicketId
		}
		if b.Gate != nil {
			in.Gate = *b.Gate
		}
		// Every scan, invalid ones too, writes a checkins row: this bounds them.
		if !s.checkinLimit.Allow(in.Actor.User.String()+in.Actor.Key.String()+req.Event.String(), time.Now()) {
			return errTooManyScans
		}
		r, err := checkin.CheckIn(ctx, tx, org.ID, req.Event, in)
		out = CreateCheckin200JSONResponse{Result: CheckinResultResult(r.Result), CheckedIn: r.CheckedIn, Sold: r.Sold}
		if r.Ticket != nil {
			out.Ticket.Set(toCheckinTicket(*r.Ticket))
		} else {
			out.Ticket.SetNull()
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *server) SearchCheckinTickets(ctx context.Context, req SearchCheckinTicketsRequestObject) (SearchCheckinTicketsResponseObject, error) {
	out := SearchCheckinTickets200JSONResponse{}
	err := s.inEvent(ctx, req.Organizer, req.Event, catalogueRead, func(_ organizers.Organizer, tx pgx.Tx) error {
		// The raw q has the spec's 120 ceiling; the 2 floor counts what's left
		// once spaces around it are dropped.
		if utf8.RuneCountInString(req.Params.Q) > 120 || utf8.RuneCountInString(strings.TrimSpace(req.Params.Q)) < 2 {
			return errInvalidQuery
		}
		list, err := checkin.Search(ctx, tx, req.Event, req.Params.Q)
		for _, t := range list {
			out = append(out, toCheckinTicket(t))
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *server) GetCheckinSummary(ctx context.Context, req GetCheckinSummaryRequestObject) (GetCheckinSummaryResponseObject, error) {
	var out GetCheckinSummary200JSONResponse
	err := s.inEvent(ctx, req.Organizer, req.Event, catalogueRead, func(_ organizers.Organizer, tx pgx.Tx) (err error) {
		out.CheckedIn, out.Sold, err = checkin.Summary(ctx, tx, req.Event)
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// toCheckinTicket carries no money: check-in staff never see what anyone paid.
func toCheckinTicket(t checkin.Ticket) CheckinTicket {
	return CheckinTicket{Id: t.ID, TicketTypeName: t.TicketTypeName, AttendeeName: t.AttendeeName, Email: t.Email,
		OrderCode: t.OrderCode, Status: CheckinTicketStatus(t.Status), CheckedInAt: nullOf(t.CheckedInAt)}
}
