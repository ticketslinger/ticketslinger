package tickets

import (
	_ "embed"
	"fmt"
	"strings"

	"github.com/signintech/gopdf"
)

//go:embed fonts/NotoSans-Regular.ttf
var notoRegular []byte

//go:embed fonts/NotoSans-Bold.ttf
var notoBold []byte

const (
	regular = "regular"
	bold    = "bold"

	pageW    = 612.0 // US Letter, points
	margin   = 72.0
	maxWidth = pageW - 2*margin // 468
	qrSide   = 288.0
	qrTop    = 204.0

	footer = "Each ticket lets one person in, once. A copy works as well as the original, so keep it to yourself."
)

// PDFTicket is one page of the PDF.
type PDFTicket struct{ Type, Attendee, QR string }

// PDFOrder is what the PDF prints: the event, then one page per ticket.
type PDFOrder struct {
	Code, Event, When, Venue string
	Tickets                  []PDFTicket
}

// newDoc starts a US Letter document with both Noto Sans faces loaded.
// It sets no document information dictionary, so no order data lands in metadata.
func newDoc() (*gopdf.GoPdf, error) {
	pdf := &gopdf.GoPdf{}
	pdf.Start(gopdf.Config{PageSize: *gopdf.PageSizeLetter})
	if err := pdf.AddTTFFontData(regular, notoRegular); err != nil {
		return nil, err
	}
	if err := pdf.AddTTFFontData(bold, notoBold); err != nil {
		return nil, err
	}
	return pdf, nil
}

// PDF renders one ticket per page.
func PDF(o PDFOrder) ([]byte, error) {
	pdf, err := newDoc()
	if err != nil {
		return nil, fmt.Errorf("tickets: pdf: %w", err)
	}
	n := len(o.Tickets)
	for i, t := range o.Tickets {
		m, err := QR(t.QR)
		if err != nil {
			return nil, fmt.Errorf("tickets: qr: %w", err)
		}
		pdf.AddPage()
		lines := []struct {
			family string
			size   float64
			y      float64
			text   string
		}{
			{bold, 22, 72, o.Event},
			{regular, 12, 102, o.When},
			{regular, 12, 120, o.Venue},
			{bold, 14, 522, fmt.Sprintf("Ticket %d of %d · %s", i+1, n, t.Type)},
			{regular, 12, 540, t.Attendee},
			{regular, 12, 558, "Order " + o.Code},
			{regular, 9, 744, footer},
		}
		for _, l := range lines {
			if err := line(pdf, l.family, l.size, l.y, l.text); err != nil {
				return nil, fmt.Errorf("tickets: pdf: %w", err)
			}
		}
		drawQR(pdf, m)
	}
	b, err := pdf.GetBytesPdfReturnErr()
	if err != nil {
		return nil, fmt.Errorf("tickets: pdf: %w", err)
	}
	return b, nil
}

// line draws text at the left margin, with runes the font lacks shown as ?
// and the whole cut to fit between the margins.
func line(pdf *gopdf.GoPdf, family string, size, y float64, text string) error {
	if text == "" {
		return nil
	}
	if err := pdf.SetFont(family, "", size); err != nil {
		return err
	}
	var b strings.Builder
	for _, r := range text {
		ok, err := pdf.IsCurrFontContainGlyph(r)
		if err != nil {
			return err
		}
		if !ok {
			r = '?'
		}
		b.WriteRune(r)
	}
	text, err := fit(pdf, b.String(), maxWidth)
	if err != nil {
		return err
	}
	pdf.SetXY(margin, y)
	return pdf.Cell(nil, text)
}

// fit returns text unchanged when it is at most max points wide in the
// current font, else cut at the last rune that fits with "..." added.
func fit(pdf *gopdf.GoPdf, text string, max float64) (string, error) {
	w, err := pdf.MeasureTextWidth(text)
	if err != nil || w <= max {
		return text, err
	}
	runes := []rune(text)
	for i := len(runes) - 1; i >= 0; i-- {
		cut := string(runes[:i]) + "..."
		w, err := pdf.MeasureTextWidth(cut)
		if err != nil {
			return "", err
		}
		if w <= max {
			return cut, nil
		}
	}
	return "...", nil
}

// drawQR draws the code as one filled rectangle per horizontal run of dark modules.
func drawQR(pdf *gopdf.GoPdf, m Matrix) {
	unit := qrSide / float64(m.Size)
	left := (pageW - qrSide) / 2
	pdf.SetFillColor(0, 0, 0)
	for y := range m.Size {
		for x := 0; x < m.Size; {
			if !m.Dark(x, y) {
				x++
				continue
			}
			start := x
			for x < m.Size && m.Dark(x, y) {
				x++
			}
			pdf.RectFromUpperLeftWithStyle(left+float64(start)*unit, qrTop+float64(y)*unit, float64(x-start)*unit, unit, "F")
		}
	}
}
