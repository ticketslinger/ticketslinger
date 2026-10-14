package httpapi

import (
	"context"
	"log/slog"

	"github.com/jackc/pgx/v5"

	"github.com/ticketslinger/ticketslinger/api/internal/audit"
	"github.com/ticketslinger/ticketslinger/api/internal/organizers"
)

var errNotConnected = &apiError{409, "not_connected", "There's no Stripe account to check yet. Connect one first."}

// providerError logs what the payment provider said and answers 503. The
// provider's errors never carry our keys.
func providerError(ctx context.Context, op string, err error) error {
	slog.ErrorContext(ctx, "payment provider", "op", op, "err", err)
	return errPaymentsUnavailable
}

func (s *server) paymentsStatus(ctx context.Context, tx pgx.Tx, org organizers.Organizer) (PaymentsStatus, error) {
	st := PaymentsStatus{Provider: None}
	if s.Payments != nil {
		st.Provider = PaymentsStatusProvider(s.Payments.Name())
	}
	err := tx.QueryRow(ctx, `select stripe_account_id is not null, stripe_charges_enabled, stripe_details_submitted from organizers where id = $1`, org.ID).
		Scan(&st.Connected, &st.ChargesEnabled, &st.DetailsSubmitted)
	return st, err
}

func (s *server) GetPayments(ctx context.Context, req GetPaymentsRequestObject) (GetPaymentsResponseObject, error) {
	var out PaymentsStatus
	err := s.inOrganizer(ctx, req.Organizer, Access{}, func(org organizers.Organizer, tx pgx.Tx) (err error) {
		out, err = s.paymentsStatus(ctx, tx, org)
		return err
	})
	if err != nil {
		return nil, err
	}
	return GetPayments200JSONResponse(out), nil
}

// ConnectPayments creates the organizer's Stripe account once and hands back
// an onboarding link. The network calls sit inside the transaction on
// purpose: the organizer row lock is what makes two clicks produce one
// account, and this runs once per organizer.
func (s *server) ConnectPayments(ctx context.Context, req ConnectPaymentsRequestObject) (ConnectPaymentsResponseObject, error) {
	var link string
	var linkErr error
	err := s.inOrganizer(ctx, req.Organizer, Access{Roles: ownerAdmin, Write: true, SessionOnly: true}, func(org organizers.Organizer, tx pgx.Tx) error {
		if s.Payments == nil {
			return errPaymentsNotConfigured
		}
		u, _, err := userFrom(ctx)
		if err != nil {
			return err
		}
		var account *string
		if err := tx.QueryRow(ctx, `select stripe_account_id from organizers where id = $1 for update`, org.ID).Scan(&account); err != nil {
			return err
		}
		if account == nil {
			id, err := s.Payments.CreateAccount(ctx, u.Email, "acct-"+org.ID.String())
			if err != nil {
				return providerError(ctx, "create account", err)
			}
			if _, err := tx.Exec(ctx, `update organizers set stripe_account_id = $2 where id = $1`, org.ID, id); err != nil {
				return err
			}
			err = audit.Write(ctx, tx, audit.Entry{Organizer: org.ID, Actor: actorFrom(ctx), Action: "organizer.stripe_connected",
				EntityType: "organizer", EntityID: org.ID, After: map[string]string{"stripe_account_id": id}})
			if err != nil {
				return err
			}
			account = &id
		}
		base := s.Config.DashboardOrigin + "/o/" + org.Slug + "/payments"
		// A failed link still commits the new account, so a retry reuses it.
		link, linkErr = s.Payments.OnboardingLink(ctx, *account, base+"?refresh=1", base+"?return=1")
		return nil
	})
	if err != nil {
		return nil, err
	}
	if linkErr != nil {
		return nil, providerError(ctx, "onboarding link", linkErr)
	}
	return ConnectPayments200JSONResponse{Url: link}, nil
}

// RefreshPayments asks the provider for the account's state and saves it.
func (s *server) RefreshPayments(ctx context.Context, req RefreshPaymentsRequestObject) (RefreshPaymentsResponseObject, error) {
	var out PaymentsStatus
	err := s.inOrganizer(ctx, req.Organizer, Access{Roles: ownerAdmin, Write: true}, func(org organizers.Organizer, tx pgx.Tx) error {
		if s.Payments == nil {
			return errPaymentsNotConfigured
		}
		var account *string
		if err := tx.QueryRow(ctx, `select stripe_account_id from organizers where id = $1 for update`, org.ID).Scan(&account); err != nil {
			return err
		}
		if account == nil {
			return errNotConnected
		}
		st, err := s.Payments.Account(ctx, *account)
		if err != nil {
			return providerError(ctx, "account", err)
		}
		if _, err := tx.Exec(ctx, `update organizers set stripe_charges_enabled = $2, stripe_details_submitted = $3 where id = $1`,
			org.ID, st.ChargesEnabled, st.DetailsSubmitted); err != nil {
			return err
		}
		out, err = s.paymentsStatus(ctx, tx, org)
		return err
	})
	if err != nil {
		return nil, err
	}
	return RefreshPayments200JSONResponse(out), nil
}
