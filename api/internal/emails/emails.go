// Package emails renders every message TicketSlinger sends, HTML and text,
// from embedded templates. The HTML goes through html/template, so
// organizer-supplied text (event, venue, ticket type, names) is escaped.
// No remote images, no tracking.
package emails

import (
	"bytes"
	"embed"
	htmltemplate "html/template"
	"regexp"
	"strconv"
	"strings"
	texttemplate "text/template"

	"github.com/google/uuid"

	"github.com/ticketslinger/ticketslinger/api/internal/mailer"
	"github.com/ticketslinger/ticketslinger/api/internal/tickets"
)

// InlineLimit is the most QR codes one email carries inline; the PDF has them all.
const InlineLimit = 10

//go:embed templates
var files embed.FS

var (
	layout      = htmltemplate.Must(htmltemplate.ParseFS(files, "templates/layout.html.tmpl"))
	ticketsHTML = htmltemplate.Must(htmltemplate.Must(layout.Clone()).ParseFS(files, "templates/tickets.html.tmpl"))
	ticketsText = texttemplate.Must(texttemplate.ParseFS(files, "templates/tickets.txt.tmpl"))
	plainHTML   = htmltemplate.Must(htmltemplate.Must(layout.Clone()).Parse(`{{define "body"}}{{range .Paras}}<p>
		{{- range $i, $line := .}}{{if $i}}<br>{{end}}{{range $line}}{{if .Link}}<a href="{{.Text}}">{{.Text}}</a>{{else}}{{.Text}}{{end}}{{end}}{{end -}}
		</p>
{{end}}{{end}}`))
)

// Ticket is one valid ticket: its type's name, the attendee (may be empty)
// and the QR value (base64url of the ticket's secret).
type Ticket struct{ Type, Attendee, QR string }

// TicketsData is everything the order confirmation shows.
type TicketsData struct {
	To, Event, When, Venue, Organizer, Code, OrderURL, LostLinkURL string
	OrderID, OrganizerID                                           uuid.UUID
	Tickets                                                        []Ticket
}

type shown struct {
	N              int
	Type, Attendee string
}

// Tickets is the order confirmation: the first InlineLimit QR codes inline
// as qr-1…qr-k, every ticket in tickets-<code>.pdf, tagged with the order
// and organizer for delivery webhooks. An order with no tickets is an error
// (tickets.PDF refuses it).
func Tickets(d TicketsData) (mailer.Message, error) {
	pdf, err := tickets.PDF(tickets.PDFOrder{Code: d.Code, Event: d.Event, When: d.When, Venue: d.Venue, Tickets: pdfTickets(d.Tickets)})
	if err != nil {
		return mailer.Message{}, err
	}
	view := struct {
		TicketsData
		Subject string
		Count   int
		Over    bool
		Shown   []shown
	}{TicketsData: d, Subject: "Your tickets for " + d.Event, Count: len(d.Tickets), Over: len(d.Tickets) > InlineLimit}
	var parts []mailer.Part
	for i, t := range d.Tickets[:min(len(d.Tickets), InlineLimit)] {
		png, err := tickets.PNG(t.QR)
		if err != nil {
			return mailer.Message{}, err
		}
		n := strconv.Itoa(i + 1)
		parts = append(parts, mailer.Part{Filename: "ticket-" + n + ".png", ContentType: "image/png", ContentID: "qr-" + n, Data: png})
		view.Shown = append(view.Shown, shown{N: i + 1, Type: t.Type, Attendee: t.Attendee})
	}
	parts = append(parts, mailer.Part{Filename: "tickets-" + d.Code + ".pdf", ContentType: "application/pdf", Data: pdf})
	var h, txt bytes.Buffer
	if err := ticketsHTML.ExecuteTemplate(&h, "layout", view); err != nil {
		return mailer.Message{}, err
	}
	if err := ticketsText.Execute(&txt, view); err != nil {
		return mailer.Message{}, err
	}
	return mailer.Message{To: d.To, Subject: view.Subject, Text: txt.String(), HTML: h.String(), Parts: parts,
		Tags: map[string]string{"order": d.OrderID.String(), "organizer": d.OrganizerID.String()}}, nil
}

func pdfTickets(ts []Ticket) []tickets.PDFTicket {
	out := make([]tickets.PDFTicket, len(ts))
	for i, t := range ts {
		out[i] = tickets.PDFTicket(t)
	}
	return out
}

type segment struct {
	Text string
	Link bool
}

var linkRe = regexp.MustCompile(`https?://\S+`)

// Plain wraps plain text in the layout: each blank-line paragraph is a <p>,
// each line break a <br>, each http(s) URL a link to itself.
func Plain(to, subject, text string) mailer.Message {
	var paras [][][]segment
	for _, p := range strings.Split(strings.TrimSpace(text), "\n\n") {
		var lines [][]segment
		for _, l := range strings.Split(strings.TrimSpace(p), "\n") {
			var segs []segment
			at := 0
			for _, m := range linkRe.FindAllStringIndex(l, -1) {
				segs = append(segs, segment{Text: l[at:m[0]]}, segment{Text: l[m[0]:m[1]], Link: true})
				at = m[1]
			}
			lines = append(lines, append(segs, segment{Text: l[at:]}))
		}
		paras = append(paras, lines)
	}
	var h bytes.Buffer
	// Can't fail: the template is fixed and the data is strings.
	_ = plainHTML.ExecuteTemplate(&h, "layout", struct {
		Subject string
		Paras   [][][]segment
	}{subject, paras})
	return mailer.Message{To: to, Subject: subject, Text: text, HTML: h.String()}
}
