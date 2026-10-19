package mailer

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"io"
	"math/big"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/textproto"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeSMTP is just enough of an SMTP server: EHLO, STARTTLS, AUTH PLAIN,
// MAIL, RCPT, DATA and QUIT. reply overrides the answer to a verb; "BODY"
// is the answer after the message.
type fakeSMTP struct {
	port      int
	implicit  bool // TLS from the first byte, as on 465
	starttls  bool // advertise STARTTLS
	reply     map[string]string
	tlsConfig *tls.Config
	roots     *x509.CertPool

	mu   sync.Mutex
	cmds []string
	data []byte
}

func newFakeSMTP(t *testing.T, implicit, starttls bool, reply map[string]string) *fakeSMTP {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeSMTP{implicit: implicit, starttls: starttls, reply: reply, roots: x509.NewCertPool(),
		tlsConfig: &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}}
	f.roots.AddCert(cert)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	f.port = l.Addr().(*net.TCPAddr).Port
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go f.serve(c)
		}
	}()
	return f
}

func (f *fakeSMTP) serve(c net.Conn) {
	defer func() { _ = c.Close() }()
	secure := f.implicit
	if secure {
		c = tls.Server(c, f.tlsConfig)
	}
	tp := textproto.NewConn(c)
	say := func(s string) { _ = tp.PrintfLine("%s", s) }
	answer := func(verb, def string) string {
		if r, ok := f.reply[verb]; ok {
			return r
		}
		return def
	}
	say("220 fake ESMTP")
	for {
		line, err := tp.ReadLine()
		if err != nil {
			return
		}
		verb := strings.ToUpper(strings.Fields(line + " x")[0])
		f.mu.Lock()
		f.cmds = append(f.cmds, verb)
		f.mu.Unlock()
		switch verb {
		case "EHLO":
			if f.starttls && !secure {
				say("250-fake\r\n250-STARTTLS\r\n250 AUTH PLAIN")
			} else {
				say("250-fake\r\n250 AUTH PLAIN")
			}
		case "STARTTLS":
			say("220 go ahead")
			tc := tls.Server(c, f.tlsConfig)
			if tc.Handshake() != nil {
				return
			}
			c, secure = tc, true
			tp = textproto.NewConn(c)
		case "AUTH":
			say(answer(verb, "235 ok"))
		case "DATA":
			r := answer(verb, "354 go ahead")
			say(r)
			if !strings.HasPrefix(r, "354") {
				continue
			}
			b, err := tp.ReadDotBytes()
			if err != nil {
				return
			}
			f.mu.Lock()
			f.data = b
			f.mu.Unlock()
			say(answer("BODY", "250 queued"))
		case "QUIT":
			say("221 bye")
			return
		default:
			say(answer(verb, "250 ok"))
		}
	}
}

func (f *fakeSMTP) got() ([]string, []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.cmds...), f.data
}

func (f *fakeSMTP) client() *SMTP {
	return &SMTP{Host: "127.0.0.1", Port: f.port, Username: "relay", Password: "hunter2-SECRET", From: "TicketSlinger <tickets@ticketslinger.com>",
		TLS: &tls.Config{ServerName: "127.0.0.1", RootCAs: f.roots, MinVersion: tls.VersionTLS12}}
}

var (
	testPNG = []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', 0, 1, 2, 0xff}
	testPDF = bytes.Repeat([]byte("%PDF-1.7 \x00\xff\r\n"), 40)
)

func testMessage(subject string) Message {
	return Message{To: "buyer@example.com", Subject: subject, Text: "Your tickets are attached. Café at 7.",
		HTML: `<p lang="en">Your tickets <img src="cid:qr-1" alt="QR code"></p>`,
		Parts: []Part{
			{Filename: "qr-1.png", ContentType: "image/png", ContentID: "qr-1", Data: testPNG},
			{Filename: "tickets.pdf", ContentType: "application/pdf", Data: testPDF},
		}}
}

type leaf struct {
	h    textproto.MIMEHeader
	body []byte // decoded
}

// leaves walks a MIME tree and returns its non-multipart parts, decoded,
// failing on any encoded line over 76 characters.
func leaves(t *testing.T, h textproto.MIMEHeader, body io.Reader) []leaf {
	t.Helper()
	raw, err := io.ReadAll(body)
	if err != nil {
		t.Fatal(err)
	}
	mt, params, err := mime.ParseMediaType(h.Get("Content-Type"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(mt, "multipart/") {
		var out []leaf
		r := multipart.NewReader(bytes.NewReader(raw), params["boundary"])
		for {
			p, err := r.NextRawPart()
			if errors.Is(err, io.EOF) {
				return out
			}
			if err != nil {
				t.Fatal(err)
			}
			out = append(out, leaves(t, p.Header, p)...)
		}
	}
	for _, line := range strings.Split(string(raw), "\n") { // the fake's ReadDotBytes turns CRLF into LF
		if len(strings.TrimSuffix(line, "\r")) > 76 {
			t.Fatalf("line of %d characters in %v", len(line), h)
		}
	}
	switch h.Get("Content-Transfer-Encoding") {
	case "base64":
		d, err := base64.StdEncoding.DecodeString(strings.NewReplacer("\r", "", "\n", "").Replace(string(raw)))
		if err != nil {
			t.Fatal(err)
		}
		return []leaf{{h, d}}
	case "quoted-printable":
		d, err := io.ReadAll(quotedprintable.NewReader(bytes.NewReader(raw)))
		if err != nil {
			t.Fatal(err)
		}
		return []leaf{{h, d}}
	}
	t.Fatalf("part %v has no known encoding", h)
	return nil
}

func TestSMTPSendsTheMessage(t *testing.T) {
	f := newFakeSMTP(t, false, true, nil)
	if err := f.client().Send(context.Background(), testMessage("Your tickets for Gala ✨")); err != nil {
		t.Fatal(err)
	}
	cmds, data := f.got()
	if got := strings.Join(cmds, " "); got != "EHLO STARTTLS EHLO AUTH MAIL RCPT DATA QUIT" {
		t.Fatalf("commands %q", got)
	}
	msg, err := mail.ReadMessage(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	subject, err := new(mime.WordDecoder).DecodeHeader(msg.Header.Get("Subject"))
	if err != nil || subject != "Your tickets for Gala ✨" {
		t.Fatalf("subject %q %v", subject, err)
	}
	if msg.Header.Get("To") != "buyer@example.com" || msg.Header.Get("MIME-Version") != "1.0" || !strings.Contains(msg.Header.Get("From"), "<tickets@ticketslinger.com>") {
		t.Fatalf("headers %v", msg.Header)
	}
	if id := msg.Header.Get("Message-Id"); !strings.HasSuffix(id, "@ticketslinger.com>") || len(id) != len("<@ticketslinger.com>")+32 {
		t.Fatalf("Message-ID %q", id)
	}
	if _, err := msg.Header.Date(); err != nil {
		t.Fatal(err)
	}
	ps := leaves(t, textproto.MIMEHeader(msg.Header), msg.Body)
	if len(ps) != 4 {
		t.Fatalf("%d parts, want text, html, png, pdf", len(ps))
	}
	want := []struct{ ct, disp string }{{"text/plain; charset=utf-8", ""}, {"text/html; charset=utf-8", ""}, {"image/png", "inline"}, {"application/pdf", "attachment"}}
	m := testMessage("")
	for i, w := range want {
		h := ps[i].h
		if h.Get("Content-Type") != w.ct || !strings.HasPrefix(h.Get("Content-Disposition"), w.disp) {
			t.Fatalf("part %d is %v, want %s %s", i, h, w.ct, w.disp)
		}
		body := ps[i].body
		switch i {
		case 0:
			if string(body) != m.Text {
				t.Fatalf("text %q", body)
			}
		case 1:
			if string(body) != m.HTML {
				t.Fatalf("html %q", body)
			}
		case 2:
			if h.Get("Content-Id") != "<qr-1>" || !bytes.Equal(body, testPNG) {
				t.Fatalf("png %v %x", h, body)
			}
		case 3:
			if h.Get("Content-Disposition") != `attachment; filename=tickets.pdf` || !bytes.Equal(body, testPDF) {
				t.Fatalf("pdf %v", h)
			}
		}
	}
}

func TestSMTPImplicitTLS(t *testing.T) {
	f := newFakeSMTP(t, true, false, nil)
	defer func(p int) { implicitTLSPort = p }(implicitTLSPort)
	implicitTLSPort = f.port
	if err := f.client().Send(context.Background(), testMessage("Hi")); err != nil {
		t.Fatal(err)
	}
	if cmds, _ := f.got(); strings.Join(cmds, " ") != "EHLO AUTH MAIL RCPT DATA QUIT" {
		t.Fatalf("commands %v", cmds)
	}
}

func TestSMTPRequiresTLS(t *testing.T) {
	t.Run("no STARTTLS", func(t *testing.T) {
		f := newFakeSMTP(t, false, false, nil)
		err := f.client().Send(context.Background(), testMessage("Hi"))
		e, ok := errors.AsType[*Error](err)
		if !ok || e.Kind != Retry || e.Problem != "config" || e.Code != "no_starttls" {
			t.Fatalf("err %#v, want Retry config no_starttls", err)
		}
		cmds, _ := f.got()
		for _, c := range cmds {
			if c == "AUTH" || c == "MAIL" {
				t.Fatalf("sent %s over plaintext: %v", c, cmds)
			}
		}
	})
	t.Run("unverified certificate", func(t *testing.T) {
		f := newFakeSMTP(t, false, true, nil)
		s := f.client()
		s.TLS = nil // system roots don't know the fake's certificate
		err := s.Send(context.Background(), testMessage("Hi"))
		if e, ok := errors.AsType[*Error](err); !ok || e.Kind != Retry {
			t.Fatalf("err %#v, want Retry", err)
		}
		cmds, _ := f.got()
		for _, c := range cmds {
			if c == "AUTH" || c == "MAIL" {
				t.Fatalf("sent %s without a verified certificate: %v", c, cmds)
			}
		}
	})
}

func TestSMTPStripsHeaderInjection(t *testing.T) {
	f := newFakeSMTP(t, false, true, nil)
	m := testMessage("Gala\r\nBcc: x@evil.test")
	m.Parts[1].Filename = "t.pdf\r\nBcc: y@evil.test"
	if err := f.client().Send(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	_, data := f.got()
	msg, err := mail.ReadMessage(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := msg.Header["Bcc"]; ok || bytes.Contains(data, []byte("\r\nBcc:")) {
		t.Fatalf("injected Bcc in %q", data)
	}
	if s, _ := new(mime.WordDecoder).DecodeHeader(msg.Header.Get("Subject")); s != "GalaBcc: x@evil.test" {
		t.Fatalf("subject %q", s)
	}
}

func TestSMTPRefusesAMalformedRecipient(t *testing.T) {
	f := newFakeSMTP(t, false, true, nil)
	m := testMessage("Hi")
	m.To = "buyer@example.com\r\nBcc: x@evil.test"
	err := f.client().Send(context.Background(), m)
	if e, ok := errors.AsType[*Error](err); !ok || e.Kind != Permanent || e.Problem != "bad_address" || err.Error() != "smtp: to" {
		t.Fatalf("err %#v, want Permanent bad_address", err)
	}
	if cmds, _ := f.got(); len(cmds) != 0 {
		t.Fatalf("talked to the server: %v", cmds)
	}
}

func TestSMTPMapsReplies(t *testing.T) {
	for _, c := range []struct {
		verb, reply string
		kind        Kind
		code, prob  string
	}{
		{"RCPT", "550 5.1.1 no such user buyer@example.com", Permanent, "550", "bad_address"},
		{"RCPT", "553 5.1.3 bad address", Permanent, "553", "bad_address"},
		{"BODY", "554 5.7.1 rejected as spam", Permanent, "554", "refused"},
		{"MAIL", "451 4.3.0 try later", Retry, "451", "unreachable"},
		{"RCPT", "452 4.2.2 mailbox full", Retry, "452", "unreachable"},
		{"AUTH", "535 5.7.8 bad credentials for relay hunter2-SECRET", Retry, "535", "config"},
	} {
		t.Run(c.verb+" "+c.reply[:3], func(t *testing.T) {
			f := newFakeSMTP(t, false, true, map[string]string{c.verb: c.reply})
			err := f.client().Send(context.Background(), testMessage("Hi"))
			e, ok := errors.AsType[*Error](err)
			if !ok || e.Kind != c.kind || e.Code != c.code || e.Problem != c.prob || e.Provider != "smtp" {
				t.Fatalf("err %#v, want kind %d %s %s", err, c.kind, c.code, c.prob)
			}
			if s := err.Error(); s != "smtp: "+c.code {
				t.Fatalf("Error() = %q, want only the code", s)
			}
		})
	}
	t.Run("nobody listening", func(t *testing.T) {
		l, _ := net.Listen("tcp", "127.0.0.1:0")
		port := l.Addr().(*net.TCPAddr).Port
		_ = l.Close()
		err := (&SMTP{Host: "127.0.0.1", Port: port, From: "a@example.com"}).Send(context.Background(), testMessage("Hi"))
		if e, ok := errors.AsType[*Error](err); !ok || e.Kind != Retry || e.Problem != "unreachable" {
			t.Fatalf("err %#v", err)
		}
	})
	t.Run("silent server times out", func(t *testing.T) {
		l, _ := net.Listen("tcp", "127.0.0.1:0")
		defer func() { _ = l.Close() }()
		go func() {
			c, err := l.Accept()
			if err == nil {
				_, _ = bufio.NewReader(c).ReadString('\n') // never greets
			}
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()
		err := (&SMTP{Host: "127.0.0.1", Port: l.Addr().(*net.TCPAddr).Port, From: "a@example.com"}).Send(ctx, testMessage("Hi"))
		if e, ok := errors.AsType[*Error](err); !ok || e.Kind != Retry || e.Problem != "unreachable" {
			t.Fatalf("err %#v", err)
		}
	})
}

func TestBuildMIMECollapsesMissingParts(t *testing.T) {
	b := buildMIME("a@example.com", Message{To: "b@example.com", Subject: "Plain", Text: "hello"}, time.Unix(0, 0), "00ff")
	msg, err := mail.ReadMessage(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	if ct := msg.Header.Get("Content-Type"); ct != "text/plain; charset=utf-8" || msg.Header.Get("Message-Id") != "<00ff@example.com>" {
		t.Fatalf("headers %v", msg.Header)
	}
	if body, _ := io.ReadAll(quotedprintable.NewReader(msg.Body)); string(body) != "hello" {
		t.Fatalf("body %q", body)
	}
}
