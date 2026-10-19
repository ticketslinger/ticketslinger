package mailer

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"log/slog"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/smtp"
	"net/textproto"
	"strconv"
	"strings"
	"time"
)

// SMTP sends through a self-hoster's own server with net/smtp. It never
// talks in plaintext: port 465 is TLS from the first byte, and any other
// port must offer STARTTLS, or nothing (no password, no message) is sent.
type SMTP struct {
	Host                     string
	Port                     int
	Username, Password, From string
	TLS                      *tls.Config // nil: verify Host, TLS 1.2 minimum
}

// implicitTLSPort is a variable so a test can use a free port.
var implicitTLSPort = 465

const smtpTimeout = 30 * time.Second

func (s *SMTP) Send(ctx context.Context, m Message) error {
	from, err := mail.ParseAddress(s.From)
	if err != nil {
		return s.fail(ctx, &Error{Provider: "smtp", Kind: Retry, Code: "from", Problem: "config"})
	}
	// One bare address, or nothing is sent: no retry can fix a malformed one.
	to, err := mail.ParseAddress(m.To)
	if err != nil {
		return &Error{Provider: "smtp", Kind: Permanent, Code: "to", Problem: "bad_address"}
	}
	m.To = to.Address
	ctx, cancel := context.WithTimeout(ctx, smtpTimeout)
	defer cancel()
	cfg := s.TLS
	if cfg == nil {
		cfg = &tls.Config{ServerName: s.Host, MinVersion: tls.VersionTLS12}
	}
	addr := net.JoinHostPort(s.Host, strconv.Itoa(s.Port))
	var conn net.Conn
	if s.Port == implicitTLSPort {
		conn, err = (&tls.Dialer{Config: cfg}).DialContext(ctx, "tcp", addr)
	} else {
		conn, err = new(net.Dialer).DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return unreachable()
	}
	defer func() { _ = conn.Close() }()
	deadline, _ := ctx.Deadline()
	_ = conn.SetDeadline(deadline)
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() }) // a cancelled send stops now
	defer stop()

	c, err := smtp.NewClient(conn, s.Host)
	if err != nil {
		return smtpError("greeting", err)
	}
	defer func() { _ = c.Close() }()
	if s.Port != implicitTLSPort {
		ok, _ := c.Extension("STARTTLS")
		if !ok {
			return s.fail(ctx, &Error{Provider: "smtp", Kind: Retry, Code: "no_starttls", Problem: "config"})
		}
		if err := c.StartTLS(cfg); err != nil {
			return smtpError("starttls", err)
		}
	}
	if s.Username != "" {
		if err := c.Auth(smtp.PlainAuth("", s.Username, s.Password, s.Host)); err != nil {
			code := "auth"
			if te, ok := errors.AsType[*textproto.Error](err); ok {
				code = strconv.Itoa(te.Code)
			}
			// The server's text and net/smtp's message are dropped: either could echo a credential.
			return s.fail(ctx, &Error{Provider: "smtp", Kind: Retry, Code: code, Problem: "config"})
		}
	}
	if err := c.Mail(from.Address); err != nil {
		return smtpError("mail", err)
	}
	if err := c.Rcpt(m.To); err != nil {
		return smtpError("rcpt", err)
	}
	w, err := c.Data()
	if err != nil {
		return smtpError("data", err)
	}
	if _, err := w.Write(buildMIME(s.From, m, time.Now(), randomID())); err != nil {
		return smtpError("data", err)
	}
	if err := w.Close(); err != nil {
		return smtpError("data", err)
	}
	_ = c.Quit() // the message is already accepted
	return nil
}

// fail logs a settings problem the self-hoster has to fix, by code only.
func (s *SMTP) fail(ctx context.Context, e *Error) error {
	slog.ErrorContext(ctx, "smtp refused the send; email waits until it is fixed", "host", s.Host, "port", s.Port, "code", e.Code)
	return e
}

func unreachable() error {
	return &Error{Provider: "smtp", Kind: Retry, Code: "network", Problem: "unreachable"}
}

// smtpError keeps only a reply's code: its text can quote addresses.
func smtpError(stage string, err error) error {
	te, ok := errors.AsType[*textproto.Error](err)
	if !ok {
		return unreachable()
	}
	e := &Error{Provider: "smtp", Kind: Retry, Code: strconv.Itoa(te.Code), Problem: "unreachable"}
	if te.Code >= 500 {
		e.Kind, e.Problem = Permanent, "refused"
		if stage == "rcpt" {
			e.Problem = "bad_address"
		}
	}
	return e
}

func randomID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// clean removes CR and LF, so no value can start a header of its own.
func clean(s string) string {
	return strings.NewReplacer("\r", "", "\n", "").Replace(s)
}

type entity struct {
	h    textproto.MIMEHeader
	body []byte
}

func textEntity(contentType, s string) entity {
	var b bytes.Buffer
	w := quotedprintable.NewWriter(&b)
	_, _ = w.Write([]byte(s))
	_ = w.Close()
	return entity{textproto.MIMEHeader{"Content-Type": {contentType}, "Content-Transfer-Encoding": {"quoted-printable"}}, b.Bytes()}
}

func fileEntity(p Part) entity {
	h := textproto.MIMEHeader{"Content-Type": {clean(p.ContentType)}, "Content-Transfer-Encoding": {"base64"}}
	disp := "attachment"
	if p.ContentID != "" {
		disp = "inline"
		h["Content-ID"] = []string{"<" + clean(p.ContentID) + ">"}
	}
	h["Content-Disposition"] = []string{mime.FormatMediaType(disp, map[string]string{"filename": clean(p.Filename)})}
	enc := base64.StdEncoding.EncodeToString(p.Data)
	var b bytes.Buffer
	for len(enc) > 76 {
		b.WriteString(enc[:76] + "\r\n")
		enc = enc[76:]
	}
	b.WriteString(enc)
	return entity{h, b.Bytes()}
}

// multi wraps parts in multipart/kind; a single part stands alone.
func multi(kind string, parts ...entity) entity {
	if len(parts) == 1 {
		return parts[0]
	}
	var b bytes.Buffer
	w := multipart.NewWriter(&b)
	for _, p := range parts {
		pw, _ := w.CreatePart(p.h)
		_, _ = pw.Write(p.body)
	}
	_ = w.Close()
	return entity{textproto.MIMEHeader{"Content-Type": {"multipart/" + kind + "; boundary=" + w.Boundary()}}, b.Bytes()}
}

// buildMIME is m as mixed{ related{ alternative{text, html}, inline images },
// attachments }, each level dropped when it would hold one part.
func buildMIME(from string, m Message, now time.Time, msgID string) []byte {
	from = clean(from)
	domain := "localhost"
	if a, err := mail.ParseAddress(from); err == nil {
		from = a.String()
		domain = a.Address[strings.LastIndexByte(a.Address, '@')+1:]
	}
	body := []entity{textEntity("text/plain; charset=utf-8", m.Text)}
	if m.HTML != "" {
		body = append(body, textEntity("text/html; charset=utf-8", m.HTML))
	}
	related := []entity{multi("alternative", body...)}
	var mixed []entity
	for _, p := range m.Parts {
		if p.ContentID != "" {
			related = append(related, fileEntity(p))
		} else {
			mixed = append(mixed, fileEntity(p))
		}
	}
	root := multi("mixed", append([]entity{multi("related", related...)}, mixed...)...)

	var b bytes.Buffer
	for _, kv := range [][2]string{
		{"From", from},
		{"To", clean(m.To)},
		{"Subject", mime.QEncoding.Encode("utf-8", clean(m.Subject))},
		{"Date", now.Format(time.RFC1123Z)},
		{"Message-ID", "<" + msgID + "@" + clean(domain) + ">"},
		{"MIME-Version", "1.0"},
	} {
		b.WriteString(kv[0] + ": " + kv[1] + "\r\n")
	}
	for _, k := range []string{"Content-Type", "Content-Transfer-Encoding"} {
		if v := root.h.Get(k); v != "" {
			b.WriteString(k + ": " + v + "\r\n")
		}
	}
	b.WriteString("\r\n")
	b.Write(root.body)
	return b.Bytes()
}
