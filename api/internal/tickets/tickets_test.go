package tickets

import (
	"bytes"
	"compress/zlib"
	"encoding/hex"
	"image/color"
	"image/png"
	"io"
	"regexp"
	"strings"
	"testing"
	"unicode/utf16"
)

func TestQRIsVersion2ForATicketValue(t *testing.T) {
	m, err := QR("AAAAAAAAAAAAAAAAAAAAAA")
	if err != nil {
		t.Fatal(err)
	}
	if m.Size != 25 {
		t.Fatalf("size = %d, want 25", m.Size)
	}
	if !m.Dark(0, 0) || !m.Dark(24, 0) || !m.Dark(0, 24) {
		t.Fatal("finder pattern corners are not dark")
	}
	if m.Dark(7, 7) {
		t.Fatal("separator at (7,7) is dark")
	}
}

func TestPNGHasAQuietZone(t *testing.T) {
	b, err := PNG("AAAAAAAAAAAAAAAAAAAAAA")
	if err != nil {
		t.Fatal(err)
	}
	img, err := png.Decode(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	if r := img.Bounds(); r.Dx() != 264 || r.Dy() != 264 {
		t.Fatalf("size = %v, want 264x264", r)
	}
	gray := func(x, y int) uint8 { return color.GrayModel.Convert(img.At(x, y)).(color.Gray).Y }
	if gray(0, 0) != 255 || gray(31, 31) != 255 {
		t.Fatal("quiet zone is not white")
	}
	if gray(32, 32) != 0 {
		t.Fatal("first module is not black")
	}
}

func order(attendees ...string) PDFOrder {
	o := PDFOrder{Code: "TS-7QK2M", Event: "Spring Social", When: "Sat 2 May 2026, 7:00 pm", Venue: "The Old Hall"}
	for _, a := range attendees {
		o.Tickets = append(o.Tickets, PDFTicket{Type: "General", Attendee: a, QR: "AAAAAAAAAAAAAAAAAAAAAA"})
	}
	return o
}

var pageRe = regexp.MustCompile(`/Type\s*/Page\b`)

func TestPDFHasOnePagePerTicket(t *testing.T) {
	b, err := PDF(order("Ada Lovelace", "Grace Hopper", "Alan Turing"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(b, []byte("%PDF-")) {
		t.Fatal("missing %PDF- header")
	}
	if !bytes.HasSuffix(bytes.TrimRight(b, " \r\n\t"), []byte("%%EOF")) {
		t.Fatal("missing EOF marker")
	}
	if n := len(pageRe.FindAll(b, -1)); n != 3 {
		t.Fatalf("pages = %d, want 3", n)
	}
}

func TestPDFRefusesAnOrderWithNoTickets(t *testing.T) {
	if b, err := PDF(order()); err == nil {
		t.Fatalf("got %d bytes and no error for an order with no tickets", len(b))
	}
}

func TestFitCutsAtTheMargin(t *testing.T) {
	pdf, err := newDoc()
	if err != nil {
		t.Fatal(err)
	}
	if err := pdf.SetFont(bold, "", 22); err != nil {
		t.Fatal(err)
	}
	got, err := fit(pdf, strings.Repeat("W", 70), 468)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(got, "...") {
		t.Fatalf("%q does not end in ...", got)
	}
	w, err := pdf.MeasureTextWidth(got)
	if err != nil {
		t.Fatal(err)
	}
	if w > 468 {
		t.Fatalf("width %.1f > 468", w)
	}
	if got, _ := fit(pdf, "Spring Social", 468); got != "Spring Social" {
		t.Fatalf("short line changed to %q", got)
	}
}

var streamRe = regexp.MustCompile(`(?s)stream\r?\n(.*?)endstream`)

// streams returns every stream body, inflated when it is Flate-encoded.
func streams(b []byte) [][]byte {
	var out [][]byte
	for _, m := range streamRe.FindAllSubmatch(b, -1) {
		body := m[1]
		if r, err := zlib.NewReader(bytes.NewReader(body)); err == nil {
			if inflated, err := io.ReadAll(r); err == nil {
				body = inflated
			}
		}
		out = append(out, body)
	}
	return out
}

func TestPDFKeepsNonLatinNames(t *testing.T) {
	b, err := PDF(order("Łukasz Дмитрий Zoë 李"))
	if err != nil {
		t.Fatal(err)
	}
	all := bytes.Join(streams(b), []byte("\n"))
	for _, cp := range []string{"0141", "0414", "00EB"} {
		if !regexp.MustCompile(`<[0-9A-F]{4}><[0-9A-F]{4}><` + cp + `>`).Match(all) {
			t.Errorf("no ToUnicode entry for U+%s", cp)
		}
	}
	if regexp.MustCompile(`<[0-9A-F]{4}><[0-9A-F]{4}><674E>`).Match(all) {
		t.Error("李 was embedded instead of replaced with ?")
	}
	if !regexp.MustCompile(`<[0-9A-F]{4}><[0-9A-F]{4}><003F>`).Match(all) {
		t.Error("李 was not replaced with ?")
	}
	o := order("Ada")
	o.Event = `Night (late) \ show`
	if _, err := PDF(o); err != nil {
		t.Fatal(err)
	}
}

func utf16Hex(s string) string {
	var buf bytes.Buffer
	for _, u := range utf16.Encode([]rune(s)) {
		buf.WriteByte(byte(u >> 8))
		buf.WriteByte(byte(u))
	}
	return strings.ToUpper(hex.EncodeToString(buf.Bytes()))
}

func TestPDFHasNoOrderMetadata(t *testing.T) {
	o := order("Ada Lovelace")
	b, err := PDF(o)
	if err != nil {
		t.Fatal(err)
	}
	// Everything outside stream bodies: the object dictionaries and trailer.
	outside := streamRe.ReplaceAll(b, nil)
	for _, key := range []string{"/Info", "/Author", "/Title", "/Subject", "/Keywords"} {
		if bytes.Contains(outside, []byte(key)) {
			t.Errorf("PDF has %s", key)
		}
	}
	for _, v := range []string{o.Code, o.Event, o.Venue, "Ada Lovelace", o.Tickets[0].QR} {
		if bytes.Contains(outside, []byte(v)) || bytes.Contains(bytes.ToUpper(outside), []byte(utf16Hex(v))) {
			t.Errorf("PDF carries %q outside page content", v)
		}
	}
}
