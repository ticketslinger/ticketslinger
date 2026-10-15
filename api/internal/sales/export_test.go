package sales

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var OrderToken = orderToken

var RequestRefundForTest = requestRefund

func FinishRefundForTest(s *Service, ctx context.Context, tx pgx.Tx, org, refund uuid.UUID, status string) error {
	return s.finishRefund(ctx, tx, org, refund, status)
}
