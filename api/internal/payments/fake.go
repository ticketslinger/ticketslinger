package payments

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"sync"

	"github.com/ticketslinger/ticketslinger/api/internal/plugin"
)

// Fake is the provider for tests and test mode: no network, and ids derived
// from the idempotency key, so the same key returns the same object, as
// Stripe does. An account can charge once its onboarding link was handed out.
type Fake struct {
	mu        sync.Mutex
	accounts  map[string]bool // ids created
	onboarded map[string]bool
	intents   map[string]plugin.PaymentIntent // by idempotency key
	latest    *plugin.PaymentIntent
	refunds   map[string]bool // ids made
	refundLog []plugin.RefundInput
	fail      error
}

var _ plugin.PaymentProvider = (*Fake)(nil)

func NewFake() *Fake {
	return &Fake{accounts: map[string]bool{}, onboarded: map[string]bool{}, intents: map[string]plugin.PaymentIntent{}, refunds: map[string]bool{}}
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

// failed returns and clears the error set by FailNext. Hold f.mu.
func (f *Fake) failed() error {
	err := f.fail
	f.fail = nil
	return err
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
	if err := f.failed(); err != nil {
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

func (f *Fake) Refund(_ context.Context, in plugin.RefundInput) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.failed(); err != nil {
		return "", err
	}
	id := fakeID("re_fake", in.IdempotencyKey)
	if !f.refunds[id] {
		f.refunds[id] = true
		f.refundLog = append(f.refundLog, in)
	}
	return id, nil
}

// Refunds lists every distinct refund asked for, oldest first.
func (f *Fake) Refunds() []plugin.RefundInput {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]plugin.RefundInput(nil), f.refundLog...)
}
