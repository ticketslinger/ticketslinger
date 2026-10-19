package httpapi_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ticketslinger/ticketslinger/api/internal/config"
	"github.com/ticketslinger/ticketslinger/api/internal/httpapi"
	"github.com/ticketslinger/ticketslinger/api/internal/payments"
	"github.com/ticketslinger/ticketslinger/api/internal/testdb"
)

// resendSecret is whsec_ and the base64 of 32 fixed bytes (0..31).
var resendSecret = func() string {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	return "whsec_" + base64.StdEncoding.EncodeToString(key)
}()

// svixSign is Svix's v1 signature of id.timestamp.body under secret.
func svixSign(secret, id, ts string, body []byte) string {
	key, _ := base64.StdEncoding.DecodeString(strings.TrimPrefix(secret, "whsec_"))
	m := hmac.New(sha256.New, key)
	m.Write([]byte(id + "." + ts + "."))
	m.Write(body)
	return "v1," + base64.StdEncoding.EncodeToString(m.Sum(nil))
}

func svixHeaders(id string, at time.Time, sig string) http.Header {
	h := http.Header{}
	h.Set("svix-id", id)
	h.Set("svix-timestamp", strconv.FormatInt(at.Unix(), 10))
	h.Set("svix-signature", sig)
	return h
}

func postResend(t *testing.T, h http.Handler, hdr http.Header, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/v1/webhooks/resend", bytes.NewReader(body))
	maps.Copy(req.Header, hdr)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestSvixSignature(t *testing.T) {
	// The test signer agrees with Svix's published example.
	if got := svixSign("whsec_MfKQ9r8GKYqrTwjUPD8ILPZIo2LaLaSw", "msg_p5jXN8AQM9LWM0D4loKWxJek", "1614265330",
		[]byte(`{"test": 2432232314}`)); got != "v1,g0hM9SsE+OTPJTGt/tmIKtSyZlE3uFJELVlNIOLJ1OE=" {
		t.Fatalf("test signer: %s", got)
	}
	// Signed and well formed, but no event we act on: 200, without a database.
	h := httpapi.New(httpapi.Deps{Config: config.Config{ResendWebhookSecret: resendSecret}})
	body := []byte(`{"a":1}`)
	now := time.Now()
	ts := strconv.FormatInt(now.Unix(), 10)
	good := svixSign(resendSecret, "msg_1", ts, body)
	wrong := svixSign("whsec_"+base64.StdEncoding.EncodeToString(make([]byte, 32)), "msg_1", ts, body)
	flipped := bytes.Clone(body)
	flipped[5] = '2'
	noSig := svixHeaders("msg_1", now, good)
	noSig.Del("svix-signature")
	noID := svixHeaders("msg_1", now, good)
	noID.Del("svix-id")
	noTS := svixHeaders("msg_1", now, good)
	noTS.Del("svix-timestamp")
	for name, c := range map[string]struct {
		hdr  http.Header
		body []byte
		want int
	}{
		"good":                   {svixHeaders("msg_1", now, good), body, 200},
		"one wrong, one right":   {svixHeaders("msg_1", now, wrong+" "+good), body, 200},
		"a flipped byte":         {svixHeaders("msg_1", now, good), flipped, 400},
		"6 minutes old":          {svixHeaders("msg_1", now.Add(-6*time.Minute), svixSign(resendSecret, "msg_1", strconv.FormatInt(now.Add(-6*time.Minute).Unix(), 10), body)), body, 400},
		"6 minutes ahead":        {svixHeaders("msg_1", now.Add(6*time.Minute), svixSign(resendSecret, "msg_1", strconv.FormatInt(now.Add(6*time.Minute).Unix(), 10), body)), body, 400},
		"no signature":           {noSig, body, 400},
		"no id":                  {noID, body, 400},
		"no timestamp":           {noTS, body, 400},
		"none of the list":       {svixHeaders("msg_1", now, wrong+" v1,AAAA "+strings.Replace(good, "v1,", "v2,", 1)), body, 400},
		"another message's id":   {svixHeaders("msg_2", now, good), body, 400},
		"too big, signed or not": {svixHeaders("msg_1", now, svixSign(resendSecret, "msg_1", ts, bytes.Repeat([]byte(" "), 256<<10+1))), bytes.Repeat([]byte(" "), 256<<10+1), 400},
	} {
		rec := postResend(t, h, c.hdr, c.body)
		if rec.Code != c.want {
			t.Errorf("%s: %d %s, want %d", name, rec.Code, rec.Body, c.want)
		}
		if c.want == 400 && !strings.Contains(rec.Body.String(), `"bad_signature"`) {
			t.Errorf("%s: body %s, want bad_signature", name, rec.Body)
		}
	}
}

func TestResendWebhookIsOffWithoutASecret(t *testing.T) {
	rec := postResend(t, httpapi.New(httpapi.Deps{}), nil, []byte(`{}`))
	if rec.Code != 404 || !strings.Contains(rec.Body.String(), "not_found") {
		t.Fatalf("got %d %s, want the API's own 404", rec.Code, rec.Body)
	}
}

func TestResendWebhookMovesTheStatus(t *testing.T) {
	s := newShopFor(t, nil, payments.NewFake(), func(c *config.Config) { c.ResendWebhookSecret = resendSecret })
	owner := testdb.Owner(t, s.pool)
	ctx := context.Background()
	var org string
	if err := owner.QueryRow(ctx, `select id::text from organizers where slug = 'acme'`).Scan(&org); err != nil {
		t.Fatal(err)
	}
	n, as := 0, org // as: the organizer tag sent
	post := func(typ, order string, signed bool) *http.Response {
		t.Helper()
		n++
		body, _ := json.Marshal(map[string]any{"type": typ, "created_at": time.Now().UTC(), "data": map[string]any{
			"created_at": time.Now().UTC(), "email_id": "56761188-7520-42d8-8898-ff6fc54ce618", "to": []string{"buyer@example.com"},
			"tags": map[string]string{"order": order, "organizer": as}}})
		id := "msg_" + strconv.Itoa(n)
		sig := svixSign(resendSecret, id, strconv.FormatInt(time.Now().Unix(), 10), body)
		if !signed {
			sig = "v1,c2lnbmVkIGJ5IHNvbWVvbmUgZWxzZQ=="
		}
		req, _ := http.NewRequest("POST", s.srv.URL+"/v1/webhooks/resend", bytes.NewReader(body))
		req.Header = svixHeaders(id, time.Now(), sig)
		res, err := s.srv.Client().Do(req) // not in the spec: Resend's own format
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = res.Body.Close() })
		return res
	}
	status := func(order string) (st string) {
		t.Helper()
		if err := owner.QueryRow(ctx, `select coalesce(mail_status, '') from orders where id = $1`, order).Scan(&st); err != nil {
			t.Fatal(err)
		}
		return st
	}
	for i, from := range []string{"queued", "failing", "sent"} {
		order := s.buyPaid(1, "d"+strconv.Itoa(i)+"@example.com", "evt_d"+strconv.Itoa(i))
		if _, err := owner.Exec(ctx, `update orders set mail_status = $2 where id = $1`, order, from); err != nil {
			t.Fatal(err)
		}
		s.must(post("email.delivered", order, true), 200)
		if st := status(order); st != "delivered" {
			t.Errorf("delivered from %s: %s", from, st)
		}
	}

	order := s.buyPaid(1, "b@example.com", "evt_b")
	if st := status(order); st != "queued" {
		t.Fatalf("a paid order's mail is %q, want queued", st)
	}
	if res := post("email.bounced", order, false); res.StatusCode != 400 || s.errorCode(res) != "bad_signature" || status(order) != "queued" {
		t.Fatalf("unsigned: %d, mail %s", res.StatusCode, status(order))
	}
	s.must(post("email.opened", order, true), 200)
	s.must(post("email.delivered", "not-a-uuid", true), 200)
	if st := status(order); st != "queued" {
		t.Fatalf("after events we don't act on: %s", st)
	}
	// Tags are believed only together: another organizer's tag with this
	// order finds nothing, and the nil UUID is no order of ours.
	s.createOrganizer(s.owner, "other")
	if err := owner.QueryRow(ctx, `select id::text from organizers where slug = 'other'`).Scan(&as); err != nil {
		t.Fatal(err)
	}
	s.must(post("email.bounced", order, true), 200)
	as = org
	s.must(post("email.bounced", "00000000-0000-0000-0000-000000000000", true), 200)
	if st := status(order); st != "queued" {
		t.Fatalf("after another organizer's tag: %s", st)
	}
	s.must(post("email.bounced", order, true), 200)
	if st := status(order); st != "bounced" {
		t.Fatalf("bounced: %s", st)
	}
	s.must(post("email.delivered", order, true), 200)
	if st := status(order); st != "bounced" {
		t.Fatalf("a late delivered undid the bounce: %s", st)
	}
	s.must(post("email.complained", order, true), 200)
	if st := status(order); st != "complained" {
		t.Fatalf("complained: %s", st)
	}
	s.must(post("email.bounced", order, true), 200)
	if st := status(order); st != "complained" {
		t.Fatalf("a late bounced undid the complaint: %s", st)
	}

	// Resend suppresses an address that bounced before: that email never
	// arrives either, so it reads as a bounce.
	sup := s.buyPaid(1, "s@example.com", "evt_s")
	s.must(post("email.suppressed", sup, true), 200)
	if st := status(sup); st != "bounced" {
		t.Fatalf("suppressed: %s", st)
	}
}
