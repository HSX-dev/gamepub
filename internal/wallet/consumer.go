package wallet

import (
	"context"
	"encoding/json"
	"errors"

	"gamepub/internal/events"
	"gamepub/pkg/mq"
)

// HandleOrderPaid 消费 order.paid：充值订单支付成功后为用户入账平台币。
// 幂等由流水唯一索引 (recharge, order_no) 保证，重复投递只会返回 Duplicated。
func (s *Service) HandleOrderPaid(ctx context.Context, msg mq.Message) error {
	var evt events.OrderPaid
	if err := json.Unmarshal(msg.Value, &evt); err != nil {
		return mq.Permanent(err)
	}
	if err := evt.Validate(); err != nil {
		return mq.Permanent(err)
	}
	if evt.Coin == 0 {
		return nil
	}
	res, err := s.Credit(ctx, ChangeRequest{
		UserID: evt.UserID, Amount: evt.Coin, BizType: BizRecharge, BizID: evt.OrderNo,
		Remark: "recharge " + evt.ProductID,
	})
	switch {
	case errors.Is(err, ErrIdempotencyConflict), errors.Is(err, ErrInvalidAmount),
		errors.Is(err, ErrInvalidBiz), errors.Is(err, ErrInvalidArgument):
		return mq.Permanent(err)
	case err != nil:
		return err
	}
	if res.Duplicated {
		s.logger.InfoContext(ctx, "wallet: duplicate order.paid ignored", "order_no", evt.OrderNo)
	}
	return nil
}
