package emails_test

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/ticketslinger/ticketslinger/api/internal/emails"
	"github.com/ticketslinger/ticketslinger/api/internal/mailer"
)

func data(n int) emails.TicketsData {
	d := emails.TicketsData{To: "buyer@example.com", Event: "Spring Social", When: "Oct 14, 2026, 7:00 PM", Venue: "The Hall",
		Organizer: "Acme", Code: "TS-ABCDE", OrderURL: "https://tickets.example/orders/TS-ABCDE?t=tok",
		LostLinkURL: "https://tickets.example/acme/spring-social/order-link", OrderID: uuid.New(), OrganizerID: uuid.New()}
	for i := range n {
		d.Tickets = append(d.Tickets, emails.Ticket{Type: "General", Attendee: fmt.Sprintf("Guest %d", i+1), QR: fmt.Sprintf("%022d", i)})
	}
	return d
}

var linkRe = regexp.MustCompile(`https?://\S+`)

func inline(m mailer.Message) (ids []string, attachments []string) {
	for _, p := range m.Parts {
		if p.ContentID != "" {
			ids = append(ids, p.ContentID)
		} else {
			attachments = append(attachments, p.Filename)
		}
	}
	return ids, attachments
}

func TestTicketEmailHasCodesPDFAndLink(t *testing.T) {
	d := data(3)
	m, err := emails.Tickets(d)
	if err != nil {
		t.Fatal(err)
	}
	if m.To != d.To || m.Subject != "Your tickets for Spring Social" {
		t.Errorf("to %q subject %q", m.To, m.Subject)
	}
	ids, att := inline(m)
	if !slices.Equal(ids, []string{"qr-1", "qr-2", "qr-3"}) || !slices.Equal(att, []string{"tickets-TS-ABCDE.pdf"}) {
		t.Errorf("inline %v attachments %v", ids, att)
	}
	for _, p := range m.Parts {
		want := "application/pdf"
		if p.ContentID != "" {
			want = "image/png"
			if p.Filename != "ticket-"+strings.TrimPrefix(p.ContentID, "qr-")+".png" {
				t.Errorf("inline filename %q", p.Filename)
			}
		}
		if p.ContentType != want || len(p.Data) == 0 {
			t.Errorf("part %s: %s, %d bytes", p.Filename, p.ContentType, len(p.Data))
		}
	}
	for _, s := range []string{`src="cid:qr-1"`, `alt="QR code for ticket 1, General"`, `href="https://tickets.example/orders/TS-ABCDE?t=tok"`} {
		if !strings.Contains(m.HTML, s) {
			t.Errorf("HTML lacks %s", s)
		}
	}
	for _, s := range []string{"You're going to Spring Social.", "That link is the only key to your tickets. Keep it to yourself: anyone who has it can see them.",
		"Ticket 3 of 3: General", "Guest 3", "Open your order", "Bring the codes below to the door",
		"They're in the attached PDF too, one to a page, if you'd rather print them.",
		`Ask for a fresh link here: <a href="https://tickets.example/acme/spring-social/order-link" style="color:#1d4ed8;">https://tickets.example/acme/spring-social/order-link</a></p>`, "Acme sold you these tickets through TicketSlinger."} {
		if !strings.Contains(m.HTML, s) {
			t.Errorf("HTML lacks %q", s)
		}
	}
	for _, s := range []string{"You're going to Spring Social.", "Oct 14, 2026, 7:00 PM, The Hall.",
		"Your codes are in the attached PDF and on your order page. Bring them to the door, on your phone or on paper. Each one lets one person in, once.",
		"Ticket 1 of 3: General\nGuest 1", "The PDF has one ticket to a page, if you'd rather print them.",
		"That link is the only key to your tickets. Keep it to yourself: anyone who has it can see them.",
		"Lose this email? Ask for a fresh link here: https://tickets.example/acme/spring-social/order-link\n",
		"Acme sold you these tickets through TicketSlinger. Something wrong with them? Reply to this email and a person will read it."} {
		if !strings.Contains(m.Text, s) {
			t.Errorf("text lacks %q", s)
		}
	}
	if l := linkRe.FindString(m.Text); l != d.OrderURL {
		t.Errorf("first link in text %q, want the order URL", l)
	}
	if len(m.Tags) != 2 || m.Tags["order"] != d.OrderID.String() || m.Tags["organizer"] != d.OrganizerID.String() {
		t.Errorf("tags %v", m.Tags)
	}
}

func TestBigOrderEmailStaysSmall(t *testing.T) {
	m, err := emails.Tickets(data(100))
	if err != nil {
		t.Fatal(err)
	}
	ids, att := inline(m)
	if len(ids) != emails.InlineLimit || len(att) != 1 {
		t.Errorf("%d inline, %d attachments", len(ids), len(att))
	}
	over := "All 100 tickets are in the attached PDF, one to a page, and on your order page. That's a lot easier to scroll than 100 codes in one email."
	if !strings.Contains(m.Text, over) || strings.Contains(m.Text, "The PDF has one ticket to a page") || strings.Contains(m.Text, "Ticket 11 of") {
		t.Error("text doesn't send the reader to the PDF")
	}
	size := len(m.HTML) + len(m.Text)
	for _, p := range m.Parts {
		size += len(p.Data)
	}
	if size >= 2_000_000 {
		t.Errorf("%d bytes", size)
	}
}

func TestTemplatesEscapeOrganizerText(t *testing.T) {
	d := data(1)
	d.Event = "<script>alert(1)</script>\r\n\"x\""
	d.Organizer = "A & B"
	d.Tickets[0].Type = `"><img src=x>`
	m, err := emails.Tickets(d)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(m.HTML, "&lt;script&gt;") || !strings.Contains(m.HTML, "A &amp; B") || !strings.Contains(m.HTML, "&#34;x&#34;") {
		t.Error("organizer text isn't escaped")
	}
	if strings.Contains(m.HTML, "<script") || strings.Contains(m.HTML, "<img src=x") {
		t.Errorf("raw markup in HTML:\n%s", m.HTML)
	}
}

func TestPlainLinksAndEscapes(t *testing.T) {
	m := emails.Plain("a@example.com", "Hi", "Hi <you>\n\nOpen https://x.test/a?b=1&c=2")
	if m.To != "a@example.com" || m.Subject != "Hi" || m.Text != "Hi <you>\n\nOpen https://x.test/a?b=1&c=2" {
		t.Errorf("%+v", m)
	}
	for _, s := range []string{"<p>Hi &lt;you&gt;</p>", `<a href="https://x.test/a?b=1&amp;c=2">`} {
		if !strings.Contains(m.HTML, s) {
			t.Errorf("HTML lacks %s:\n%s", s, m.HTML)
		}
	}
}

func TestHTMLIsAccessible(t *testing.T) {
	m, err := emails.Tickets(data(3))
	if err != nil {
		t.Fatal(err)
	}
	for _, html := range []string{m.HTML, emails.Plain("a@example.com", "Hi", "Open https://x.test/").HTML} {
		if !strings.Contains(html, `<html lang="en">`) {
			t.Error("no lang")
		}
		for _, img := range regexp.MustCompile(`<img[^>]*>`).FindAllString(html, -1) {
			if !regexp.MustCompile(`alt="[^"]+"`).MatchString(img) {
				t.Errorf("no alt: %s", img)
			}
		}
		if regexp.MustCompile(`src="?https?:`).MatchString(html) {
			t.Error("remote src")
		}
	}
}
