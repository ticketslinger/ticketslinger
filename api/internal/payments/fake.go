package payments

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ticketslinger/ticketslinger/api/internal/plugin"
)

type fakeIntent struct {
	intent                plugin.PaymentIntent
	fee                   int64
	status                string
	refunded, feeRefunded int64
}

// Fake is the provider for tests and test mode: no network, and ids derived
// from the idempotency key, so the same key returns the same object, as
// Stripe does. An account can charge once its onboarding link was handed out.
type Fake struct {
	mu        sync.Mutex
	accounts  map[string]bool // ids created
	onboarded map[string]bool
	intents   map[string]plugin.PaymentIntent // by idempotency key
	latest    *plugin.PaymentIntent
	byID      map[string]*fakeIntent
	refunds   map[string]fakeRefund // by idempotency key, until it expires
	made      []fakeRefund          // every refund made, oldest first
	refundLog []plugin.RefundInput
	skew      time.Duration     // Advance
	lose      bool              // LoseNextRefundAnswer
	feeKeys   map[string]string // fee refund ids by idempotency key
	feeLog    []plugin.FeeRefundInput
	pending   bool
	fail      error
	links     int    // link URLs handed out
	linkAs    string // LinkReturns
	unlinked  []string
	failOn    map[string]error // FailCall
}

var _ plugin.PaymentProvider = (*Fake)(nil)

func NewFake() *Fake {
	return &Fake{accounts: map[string]bool{}, onboarded: map[string]bool{}, intents: map[string]plugin.PaymentIntent{}, byID: map[string]*fakeIntent{}, refunds: map[string]fakeRefund{}, feeKeys: map[string]string{}}
}

func (f *Fake) Name() string           { return "test" }
func (f *Fake) PublishableKey() string { return "pk_test_fake" }

// fakeID is prefix plus a short hash of the idempotency key (a random one
// when there's no key). Letters and digits only, as acct_ ids must be.
func fakeID(prefix, key string) string {
	if key == "" {
		key = rand.Text()
	}
	h := sha256.Sum256([]byte(key))
	return prefix + hex.EncodeToString(h[:8])
}

// FailNext makes the next call return err.
func (f *Fake) FailNext(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fail = err
}

// FailCall makes the next call of one method ("Unlink", "Account") return
// err, leaving other calls alone.
func (f *Fake) FailCall(method string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failOn == nil {
		f.failOn = map[string]error{}
	}
	f.failOn[method] = err
}

// failed returns and clears the error set by FailNext. Hold f.mu.
func (f *Fake) failed() error {
	err := f.fail
	f.fail = nil
	return err
}

// failedFor returns and clears the error FailCall set for method, else
// failed's. Hold f.mu.
func (f *Fake) failedFor(method string) error {
	if err := f.failOn[method]; err != nil {
		delete(f.failOn, method)
		return err
	}
	return f.failed()
}

func (f *Fake) CreateAccount(_ context.Context, key string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.failed(); err != nil {
		return "", err
	}
	id := fakeID("acct_fake", key)
	f.accounts[id] = true
	return id, nil
}

// Accounts counts the distinct accounts created.
func (f *Fake) Accounts() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.accounts)
}

var errNoAccount = errors.New("fake: no such account")

// OnboardingLink "completes" onboarding at once and sends the organizer
// straight back to returnURL. Any acct_fake id is known, even one created
// before a restart.
func (f *Fake) OnboardingLink(_ context.Context, account, _, returnURL string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.failed(); err != nil {
		return "", err
	}
	if !strings.HasPrefix(account, "acct_fake") {
		return "", errNoAccount
	}
	f.onboarded[account] = true
	return returnURL, nil
}

func (f *Fake) Account(_ context.Context, account string) (plugin.AccountStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.failedFor("Account"); err != nil {
		return plugin.AccountStatus{}, err
	}
	if !strings.HasPrefix(account, "acct_fake") {
		return plugin.AccountStatus{}, errNoAccount
	}
	done := f.onboarded[account]
	return plugin.AccountStatus{ChargesEnabled: done, DetailsSubmitted: done}, nil
}

func (f *Fake) CreatePaymentIntent(_ context.Context, in plugin.PaymentIntentInput) (plugin.PaymentIntent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.failed(); err != nil {
		return plugin.PaymentIntent{}, err
	}
	pi, ok := f.intents[in.IdempotencyKey]
	if !ok || in.IdempotencyKey == "" {
		id := fakeID("pi_fake", in.IdempotencyKey)
		pi = plugin.PaymentIntent{ID: id, ClientSecret: id + "_secret_" + rand.Text(), Account: in.Account, AmountCents: in.AmountCents}
		f.intents[in.IdempotencyKey] = pi
		f.byID[id] = &fakeIntent{intent: pi, fee: in.ApplicationFeeCents, status: "requires_payment_method"}
	}
	f.latest = &pi
	return pi, nil
}

// Latest is the newest PaymentIntent.
func (f *Fake) Latest() (plugin.PaymentIntent, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.latest == nil {
		return plugin.PaymentIntent{}, false
	}
	return *f.latest, true
}

func tooMuch(op string) error {
	return &plugin.ProviderError{Op: "fake: " + op, Refused: true, Status: 400, Code: "amount_too_large"}
}

type fakeRefund struct {
	r      plugin.Refund
	intent string
	at     time.Time
}

// keyWindow is how long Stripe remembers an idempotency key.
const keyWindow = 24 * time.Hour

func (f *Fake) now() time.Time { return time.Now().Add(f.skew) }

// Advance moves the fake's clock on, so idempotency keys older than 24 hours
// are forgotten, as Stripe forgets them.
func (f *Fake) Advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.skew += d
}

// LoseNextRefundAnswer makes the next refund go through but its answer
// never arrive: the caller sees a timeout.
func (f *Fake) LoseNextRefundAnswer() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lose = true
}

func (f *Fake) Refund(_ context.Context, in plugin.RefundInput) (plugin.Refund, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.failedFor("Refund"); err != nil {
		return plugin.Refund{}, err
	}
	if r, ok := f.refunds[in.IdempotencyKey]; ok && in.IdempotencyKey != "" && f.now().Sub(r.at) < keyWindow {
		return r.r, nil
	}
	pi := f.byID[in.PaymentIntentID]
	if pi == nil || in.AmountCents <= 0 || pi.refunded+in.AmountCents > pi.intent.AmountCents {
		return plugin.Refund{}, tooMuch("refund")
	}
	pi.refunded += in.AmountCents
	md := map[string]string{}
	for k, v := range in.Metadata {
		md[k] = v
	}
	// A forgotten key makes a new refund with a new id, as at Stripe.
	r := plugin.Refund{ID: fakeID("re_fake", in.IdempotencyKey+"#"+strconv.Itoa(len(f.made))), Status: "succeeded", AmountCents: in.AmountCents, Metadata: md}
	if f.pending {
		r.Status = "pending"
	}
	fr := fakeRefund{r: r, intent: in.PaymentIntentID, at: f.now()}
	f.refunds[in.IdempotencyKey] = fr
	f.made = append(f.made, fr)
	f.refundLog = append(f.refundLog, in)
	if f.lose {
		f.lose = false
		return plugin.Refund{}, &plugin.ProviderError{Op: "fake: refund", Cause: "timeout"}
	}
	return r, nil
}

// ListRefunds is every refund made on the intent, newest first.
func (f *Fake) ListRefunds(_ context.Context, _, intentID string) ([]plugin.Refund, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.failedFor("ListRefunds"); err != nil {
		return nil, err
	}
	var out []plugin.Refund
	for i := len(f.made) - 1; i >= 0; i-- {
		if f.made[i].intent == intentID {
			out = append(out, f.made[i].r)
		}
	}
	return out, nil
}

func (f *Fake) RefundApplicationFee(_ context.Context, in plugin.FeeRefundInput) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.failedFor("RefundApplicationFee"); err != nil {
		return "", err
	}
	if id, ok := f.feeKeys[in.IdempotencyKey]; ok && in.IdempotencyKey != "" {
		return id, nil
	}
	pi := f.byID[in.PaymentIntentID]
	if pi == nil || in.AmountCents <= 0 {
		return "", tooMuch("fee refund")
	}
	if left := pi.fee - pi.feeRefunded; left < in.AmountCents {
		// As Stripe's fee read: done when it's all back, or this share is.
		if pi.fee > 0 && (left == 0 || pi.feeRefunded >= in.AmountCents) {
			return "", plugin.ErrAlreadyRefunded
		}
		return "", tooMuch("fee refund")
	}
	pi.feeRefunded += in.AmountCents
	id := fakeID("fr_fake", in.IdempotencyKey)
	f.feeKeys[in.IdempotencyKey] = id
	f.feeLog = append(f.feeLog, in)
	return id, nil
}

func (f *Fake) PaymentIntentStatus(_ context.Context, _, intentID string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.failedFor("PaymentIntentStatus"); err != nil {
		return "", err
	}
	pi := f.byID[intentID]
	if pi == nil {
		return "", &plugin.ProviderError{Op: "fake: payment intent status", Refused: true, Status: 404, Code: "resource_missing"}
	}
	return pi.status, nil
}

// CancelPaymentIntent refuses an intent that moved on, as Stripe does;
// cancelling a cancelled one again answers nil, like an idempotent replay.
func (f *Fake) CancelPaymentIntent(_ context.Context, _, intentID, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.failedFor("CancelPaymentIntent"); err != nil {
		return err
	}
	pi := f.byID[intentID]
	if pi == nil {
		return &plugin.ProviderError{Op: "fake: cancel payment intent", Refused: true, Status: 404, Code: "resource_missing"}
	}
	switch pi.status {
	case "canceled":
		return nil
	case "succeeded", "processing":
		return &plugin.ProviderError{Op: "fake: cancel payment intent", Refused: true, Status: 400, Code: "payment_intent_unexpected_state"}
	}
	pi.status = "canceled"
	return nil
}

// RefundsPending makes later refunds answer pending instead of succeeded.
func (f *Fake) RefundsPending(on bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pending = on
}

// Refunded is what the intent has had refunded so far, and how much of that
// is our fee.
func (f *Fake) Refunded(intentID string) (amountCents, feeCents int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if pi := f.byID[intentID]; pi != nil {
		return pi.refunded, pi.feeRefunded
	}
	return 0, 0
}

// SetIntentStatus sets the status PaymentIntentStatus reports.
func (f *Fake) SetIntentStatus(intentID, status string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if pi := f.byID[intentID]; pi != nil {
		pi.status = status
	}
}

// Cancelled reports whether the intent was cancelled.
func (f *Fake) Cancelled(intentID string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	pi := f.byID[intentID]
	return pi != nil && pi.status == "canceled"
}

// FeeRefunds lists every distinct fee refund asked for, oldest first.
func (f *Fake) FeeRefunds() []plugin.FeeRefundInput {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]plugin.FeeRefundInput(nil), f.feeLog...)
}

// Refunds lists every distinct refund asked for, oldest first.
func (f *Fake) Refunds() []plugin.RefundInput {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]plugin.RefundInput(nil), f.refundLog...)
}

// LinkURL "authorizes" at once: it sends the organizer straight back to
// redirectURI with a fresh code, as Stripe does when they approve.
func (f *Fake) LinkURL(state, redirectURI, _ string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.links++
	return redirectURI + "?code=fake_code_" + strconv.Itoa(f.links) + "&state=" + url.QueryEscape(state), nil
}

// LinkReturns makes every later CompleteLink return id (collision tests).
func (f *Fake) LinkReturns(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.linkAs = id
}

var errBadCode = &plugin.ProviderError{Op: "fake: oauth token", Refused: true, Status: 400, Type: "invalid_grant", Code: "invalid_grant"}

// CompleteLink turns fake_code_<n> into acct_fakelink<n>, already able to
// take payments.
func (f *Fake) CompleteLink(_ context.Context, code string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.failed(); err != nil {
		return "", err
	}
	n, ok := strings.CutPrefix(code, "fake_code_")
	if _, err := strconv.Atoi(n); !ok || err != nil {
		return "", errBadCode
	}
	id := "acct_fakelink" + n
	if f.linkAs != "" {
		id = f.linkAs
	}
	f.onboarded[id] = true
	return id, nil
}

func (f *Fake) Unlink(_ context.Context, account string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.failedFor("Unlink"); err != nil {
		return err
	}
	f.unlinked = append(f.unlinked, account)
	return nil
}

// Unlinked lists the accounts Unlink was called for, oldest first.
func (f *Fake) Unlinked() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.unlinked...)
}
