package httpapi

import (
	"bytes"
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/ticketslinger/ticketslinger/api/internal/organizers"
	"github.com/ticketslinger/ticketslinger/api/internal/reports"
)

// reportsRead is ordersRead for the same reason: reports carry money and
// buyers' details, so check-in staff don't read them (spec D22). Read keys
// pass; inOrganizerAsKey ignores roles.
var (
	reportsRead = Access{Roles: ownerAdmin}

	errUnknownExport = &apiError{400, "unknown_export", "Pick orders.csv, attendees.csv or checkins.csv."}
)

func (s *server) GetEventReport(ctx context.Context, req GetEventReportRequestObject) (GetEventReportResponseObject, error) {
	var r reports.Report
	err := s.inOrganizer(ctx, req.Organizer, reportsRead, func(_ organizers.Organizer, tx pgx.Tx) (err error) {
		r, err = reports.Event(ctx, tx, req.Event)
		return err
	})
	if err != nil {
		return nil, catalogueError(err)
	}
	out := GetEventReport200JSONResponse{Currency: EventReportCurrencyUSD, GrossCents: r.GrossCents, RefundedCents: r.RefundedCents, PlatformFeeCents: r.PlatformFeeCents,
		TaxCents: r.TaxCents, NetCents: r.NetCents, Orders: r.Orders, Sold: r.Sold, CheckedIn: r.CheckedIn,
		TicketTypes: []ReportTicketType{}, Quotas: []ReportQuota{}, SalesByDay: []ReportDay{}, PromoCodes: []ReportPromoCode{}}
	for _, t := range r.TicketTypes {
		out.TicketTypes = append(out.TicketTypes, ReportTicketType{Id: t.ID, Name: t.Name, Sold: t.Sold, CheckedIn: t.CheckedIn})
	}
	for _, q := range r.Quotas {
		out.Quotas = append(out.Quotas, ReportQuota{Id: q.ID, Name: q.Name, Size: q.Size, Remaining: q.Remaining})
	}
	for _, d := range r.SalesByDay {
		out.SalesByDay = append(out.SalesByDay, ReportDay{Day: d.Day, Orders: d.Orders, Tickets: d.Tickets, GrossCents: d.GrossCents})
	}
	for _, p := range r.PromoCodes {
		out.PromoCodes = append(out.PromoCodes, ReportPromoCode{Code: p.Code, UsedCount: p.UsedCount, MaxUses: nullOf(p.MaxUses), DiscountCents: p.DiscountCents})
	}
	return out, nil
}

// ExportEventCsv builds the file inside the transaction, then sends it, so a
// failure midway is an error status rather than half a file. privateAnswers
// sets no-store: the files hold buyers' names and emails.
func (s *server) ExportEventCsv(ctx context.Context, req ExportEventCsvRequestObject) (ExportEventCsvResponseObject, error) {
	var buf bytes.Buffer
	var name string
	err := s.inOrganizer(ctx, req.Organizer, reportsRead, func(_ organizers.Organizer, tx pgx.Tx) (err error) {
		name, err = reports.WriteCSV(ctx, tx, req.Event, string(req.Kind), &buf)
		return err
	})
	if errors.Is(err, reports.ErrUnknownKind) {
		return nil, errUnknownExport
	}
	if err != nil {
		return nil, catalogueError(err)
	}
	return ExportEventCsv200TextcsvCharsetUtf8Response{Body: &buf, ContentLength: int64(buf.Len()),
		Headers: ExportEventCsv200ResponseHeaders{ContentDisposition: `attachment; filename="` + name + `"`}}, nil
}
