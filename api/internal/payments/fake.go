package payments

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"sync"

	"github.com/ticketslinger/ticketslinger/api/internal/plugin"
)

// Fake is the provider for tests and test mode: no network, predictable ids
// (acct_fake1, pi_fake1, re_fake1), and an account can charge once its
// onboarding link has been handed out.
type Fake struct {
	mu       sync.Mutex
	accounts map[string]bool // id -> onboarded
	created  int
	intents  []plugin.PaymentIntent
	refunds  []plugin.RefundInput
	fail     error
}

var _ plugin.PaymentProvider = (*Fake)(nil)

func NewFake() *Fake { return &Fake{accounts: map[string]bool{}} }

func (f *Fake) Name() string           { return "test" }
func (f *Fake) PublishableKey() string { return "pk_test_fake" }

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

func (f *Fake) CreateAccount(context.Context, string, string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.failed(); err != nil {
		return "", err
	}
	f.created++
	id := fmt.Sprintf("acct_fake%d", f.created)
	f.accounts[id] = false
	return id, nil
}

// Accounts counts CreateAccount calls.
func (f *Fake) Accounts() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.created
}

// OnboardingLink "completes" onboarding at once and sends the organizer
// straight back to returnURL.
func (f *Fake) OnboardingLink(_ context.Context, account, _, returnURL string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.failed(); err != nil {
		return "", err
	}
	if _, ok := f.accounts[account]; !ok {
		return "", errors.New("fake: no such account")
	}
	f.accounts[account] = true
	return returnURL, nil
}

func (f *Fake) Account(_ context.Context, account string) (plugin.AccountStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.failed(); err != nil {
		return plugin.AccountStatus{}, err
	}
	done, ok := f.accounts[account]
	if !ok {
		return plugin.AccountStatus{}, errors.New("fake: no such account")
	}
	return plugin.AccountStatus{ChargesEnabled: done, DetailsSubmitted: done}, nil
}

func (f *Fake) CreatePaymentIntent(_ context.Context, in plugin.PaymentIntentInput) (plugin.PaymentIntent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.failed(); err != nil {
		return plugin.PaymentIntent{}, err
	}
	id := fmt.Sprintf("pi_fake%d", len(f.intents)+1)
	pi := plugin.PaymentIntent{ID: id, ClientSecret: id + "_secret_" + rand.Text(), Account: in.Account, AmountCents: in.AmountCents}
	f.intents = append(f.intents, pi)
	return pi, nil
}

// Latest is the newest PaymentIntent.
func (f *Fake) Latest() (plugin.PaymentIntent, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.intents) == 0 {
		return plugin.PaymentIntent{}, false
	}
	return f.intents[len(f.intents)-1], true
}

func (f *Fake) Refund(_ context.Context, in plugin.RefundInput) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.failed(); err != nil {
		return "", err
	}
	f.refunds = append(f.refunds, in)
	return fmt.Sprintf("re_fake%d", len(f.refunds)), nil
}

// Refunds lists every refund asked for, oldest first.
func (f *Fake) Refunds() []plugin.RefundInput {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]plugin.RefundInput(nil), f.refunds...)
}
