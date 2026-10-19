package sales

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ticketslinger/ticketslinger/api/internal/emails"
	"github.com/ticketslinger/ticketslinger/api/internal/mailer"
)

var OrderToken = func(key []byte, order uuid.UUID) string { return orderToken(key, order, 0) }

var RequestRefundForTest = requestRefund

func FinishRefundForTest(s *Service, ctx context.Context, tx pgx.Tx, org, refund uuid.UUID, status string) error {
	return s.finishRefund(ctx, tx, org, refund, status, "")
}

func FailRefundForTest(s *Service, ctx context.Context, tx pgx.Tx, org, refund uuid.UUID, failure string) error {
	return s.failRefund(ctx, tx, org, refund, failure, "")
}

var OrderTokenAt = orderToken

var QueueTicketEmailForTest = queueTicketEmail

var QueueAnswerForTest = queueAnswer

var RefundDBErrorForTest = refundDBError

func SetTicketsEmailForTest(s *Service, fn func(emails.TicketsData) (mailer.Message, error)) {
	s.ticketsEmail = fn
}
