package order

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"

	"gorm.io/gorm"

	"gamepub/internal/events"
	"gamepub/pkg/dbx"
	"gamepub/pkg/signature"
)

var (
	ErrBadSignature    = errors.New("order: pay notify rejected")
	ErrAmountMismatch  = errors.New("order: paid amount does not match order amount")
	ErrChannelMismatch = errors.New("order: channel does not match order")
	ErrTradeReused     = errors.New("order: channel trade already used by another order")
	errConcurrentPaid  = errors.New("order: concurrent state change, retry")
)

// ReplySuccess 渠道约定的成功应答。
const ReplySuccess = "success"

type payNotify struct {
	OrderNo string `json:"order_no"`
	TradeNo string `json:"trade_no"`
	Amount  int64  `json:"amount"`
	Status  string `json:"status"`
}

// HandlePayNotify 处理支付渠道异步通知。
//
// 校验顺序：渠道白名单 → 原始报文验签 + 时间窗 + nonce 防重放 → 订单存在且渠道一致 →
// 金额一致 → 状态机 CAS（pending → paid）→ 同事务写 outbox。
// 任一业务步骤失败都释放 nonce，允许渠道用原报文重试。
func (s *Service) HandlePayNotify(ctx context.Context, channel string, body []byte, headers map[string]string) (string, error) {
	v, ok := s.verifiers[channel]
	if !ok {
		return "", ErrChannelNotSupported
	}
	h := make(map[string]string, len(headers))
	for k, val := range headers {
		h[strings.ToLower(k)] = val
	}
	nonce := h[strings.ToLower(signature.HeaderNonce)]
	if err := v.Verify(ctx, h[strings.ToLower(signature.HeaderTimestamp)], nonce, h[strings.ToLower(signature.HeaderSignature)], body); err != nil {
		s.logger.WarnContext(ctx, "pay notify rejected", "channel", channel, "reason", err)
		return "", ErrBadSignature
	}

	reply, err := s.applyPayNotify(ctx, channel, body)
	if err != nil {
		if rerr := v.Release(ctx, nonce); rerr != nil {
			s.logger.WarnContext(ctx, "pay notify: release nonce failed", "err", rerr)
		}
		return "", err
	}
	return reply, nil
}

func (s *Service) applyPayNotify(ctx context.Context, channel string, body []byte) (string, error) {
	var n payNotify
	dec := json.NewDecoder(bytes.NewReader(body))
	if err := dec.Decode(&n); err != nil {
		return "", ErrInvalidArgument
	}
	if n.Status != "SUCCESS" {
		// 非成功状态（关闭、退款中等）只应答，不推进订单状态
		s.logger.InfoContext(ctx, "pay notify ignored", "order_no", n.OrderNo, "status", n.Status)
		return ReplySuccess, nil
	}
	if !orderNoPattern.MatchString(n.OrderNo) || n.TradeNo == "" || len(n.TradeNo) > 64 {
		return "", ErrInvalidArgument
	}

	paidAt := s.now()
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var o Order
		// 回调由渠道签名鉴权，不存在用户会话，按订单号查询是预期行为
		// nosemgrep: order-lookup-without-owner
		if err := tx.Where("order_no = ?", n.OrderNo).Take(&o).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrOrderNotFound
			}
			return err
		}
		if o.Channel != channel {
			return ErrChannelMismatch
		}
		if o.Amount != n.Amount {
			// 金额不符是高危信号（篡改、渠道配置错误），不推进状态并告警
			s.logger.ErrorContext(ctx, "pay notify amount mismatch", "order_no", o.OrderNo, "order_amount", o.Amount, "paid_amount", n.Amount)
			return ErrAmountMismatch
		}
		switch o.Status {
		case StatusPaid:
			if o.TradeNo != nil && *o.TradeNo == n.TradeNo {
				return nil // 渠道重复通知：幂等应答
			}
			s.logger.ErrorContext(ctx, "order paid twice by different trades, refund required", "order_no", o.OrderNo)
			return nil
		case StatusClosed:
			s.logger.ErrorContext(ctx, "closed order got paid, refund required", "order_no", o.OrderNo)
			return nil
		}

		tradeNo := n.TradeNo
		r := tx.Model(&Order{}).Where("id = ? AND status = ?", o.ID, StatusPending).
			Updates(map[string]any{"status": StatusPaid, "trade_no": &tradeNo, "paid_at": paidAt, "updated_at": paidAt})
		if r.Error != nil {
			if dbx.IsDuplicate(r.Error) {
				s.logger.ErrorContext(ctx, "channel trade reused across orders", "order_no", o.OrderNo)
				return ErrTradeReused
			}
			return r.Error
		}
		if r.RowsAffected == 0 {
			return errConcurrentPaid
		}

		evt := events.OrderPaid{
			EventID: "order-paid:" + o.OrderNo, OrderNo: o.OrderNo, UserID: o.UserID, GameID: o.GameID,
			ProductID: o.ProductID, Amount: o.Amount, Coin: o.Coin, PaidAt: paidAt,
		}
		payload, err := json.Marshal(evt)
		if err != nil {
			return err
		}
		return tx.Create(&Outbox{
			Topic: events.TopicOrderPaid, MsgKey: strconv.FormatUint(o.UserID, 10),
			Payload: string(payload), Status: outboxPending,
		}).Error
	})
	if err != nil {
		return "", err
	}
	return ReplySuccess, nil
}
