package httpapi

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

const resendBodyLimit = 256 << 10

// resendWebhook takes Resend's word on what became of a ticket email. Like
// Stripe's, it sits outside the spec: it needs the body exactly as sent. A
// 5xx makes Resend retry; MailEvent makes repeats change nothing. With no
// webhook secret it's a 404 like any unknown route.
func (s *server) resendWebhook(w http.ResponseWriter, r *http.Request) {
	if s.Config.ResendWebhookSecret == "" {
		writeError(w, notFound())
		return
	}
	refuse := func(err error) {
		slog.WarnContext(r.Context(), "resend webhook refused", "err", err) // never the body: it names the buyer
		writeError(w, &apiError{400, "bad_signature", "Signature check failed."})
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, resendBodyLimit+1))
	if err == nil && len(body) > resendBodyLimit {
		err = errors.New("body too large")
	}
	if err != nil {
		refuse(err)
		return
	}
	// The signature first, before anything in the body is believed.
	if err := verifySvix(s.Config.ResendWebhookSecret, r.Header, body, time.Now()); err != nil {
		refuse(err)
		return
	}
	var ev struct {
		Type string `json:"type"`
		Data struct {
			CreatedAt time.Time `json:"created_at"`
			Tags      struct {
				Order     string `json:"order"`
				Organizer string `json:"organizer"`
			} `json:"tags"`
		} `json:"data"`
	}
	err = json.Unmarshal(body, &ev)
	kind, ok := strings.CutPrefix(ev.Type, "email.")
	if err != nil || !ok || (kind != "delivered" && kind != "bounced" && kind != "complained") {
		writeJSON(w, http.StatusOK, map[string]bool{"received": true}) // not one we act on
		return
	}
	org, errOrg := uuid.Parse(ev.Data.Tags.Organizer)
	order, errOrder := uuid.Parse(ev.Data.Tags.Order)
	if errOrg != nil || errOrder != nil || org == uuid.Nil || order == uuid.Nil || ev.Data.CreatedAt.IsZero() {
		writeJSON(w, http.StatusOK, map[string]bool{"received": true}) // not a ticket email of ours
		return
	}
	if err := s.Sales.MailEvent(r.Context(), org, order, kind, ev.Data.CreatedAt); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"received": true})
}

// verifySvix checks Svix's signature, which Resend's webhooks carry:
// HMAC-SHA256 over "svix-id.svix-timestamp.body", keyed with the base64
// after secret's whsec_, against any v1 entry of the space-separated
// svix-signature, signed within 5 minutes of now either way.
func verifySvix(secret string, h http.Header, body []byte, now time.Time) error {
	id, ts, sigs := h.Get("svix-id"), h.Get("svix-timestamp"), h.Get("svix-signature")
	if id == "" || ts == "" || sigs == "" {
		return errors.New("svix: missing header")
	}
	sec, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return errors.New("svix: bad timestamp")
	}
	if d := now.Sub(time.Unix(sec, 0)); d > webhookTolerance || d < -webhookTolerance {
		return errors.New("svix: timestamp out of tolerance")
	}
	b64, ok := strings.CutPrefix(secret, "whsec_")
	key, err := base64.StdEncoding.DecodeString(b64)
	if !ok || err != nil || len(key) == 0 {
		return errors.New("svix: bad secret")
	}
	m := hmac.New(sha256.New, key)
	m.Write([]byte(id + "." + ts + "."))
	m.Write(body)
	want := m.Sum(nil)
	for entry := range strings.FieldsSeq(sigs) {
		v, sig, _ := strings.Cut(entry, ",")
		got, err := base64.StdEncoding.DecodeString(sig)
		if v == "v1" && err == nil && hmac.Equal(got, want) {
			return nil
		}
	}
	return errors.New("svix: no matching signature")
}
