package order

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"gorm.io/gorm"

	"gamepub/pkg/mq"
	"gamepub/pkg/redislock"
)

// Relay 发件箱投递器：轮询 pending 事件批量发送到 Kafka。
// 语义为至少一次（发送成功但标记失败会重发），消费方以 event_id / 业务单号幂等。
type Relay struct {
	db       *gorm.DB
	pub      mq.Publisher
	locker   *redislock.Locker
	interval time.Duration
	batch    int
	logger   *slog.Logger
	now      func() time.Time
}

// NewRelay 创建投递器。
func NewRelay(db *gorm.DB, pub mq.Publisher, locker *redislock.Locker, interval time.Duration, logger *slog.Logger) *Relay {
	return &Relay{db: db, pub: pub, locker: locker, interval: interval, batch: 100, logger: logger, now: time.Now}
}

// Run 周期投递直到 ctx 取消。多副本下由分布式锁保证同一时刻只有一个副本投递，保持事件大致有序。
func (r *Relay) Run(ctx context.Context) error {
	t := time.NewTicker(r.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			r.tick(ctx)
		}
	}
}

func (r *Relay) tick(ctx context.Context) {
	const ttl = 30 * time.Second
	lock, err := r.locker.TryLock(ctx, "order:outbox-relay", ttl)
	if errors.Is(err, redislock.ErrNotAcquired) {
		return
	}
	if err != nil {
		r.logger.ErrorContext(ctx, "outbox: acquire lock failed", "err", err)
		return
	}
	defer func() {
		uctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer cancel()
		_ = lock.Unlock(uctx)
	}()
	for ctx.Err() == nil {
		n, err := r.RunOnce(ctx)
		if err != nil {
			r.logger.ErrorContext(ctx, "outbox: publish failed", "err", err)
			return
		}
		if n < r.batch {
			return
		}
		if err := lock.Refresh(ctx, ttl); err != nil {
			r.logger.WarnContext(ctx, "outbox: lock lost, stop relaying", "err", err)
			return
		}
	}
}

// RunOnce 投递一批，返回成功条数。
func (r *Relay) RunOnce(ctx context.Context) (int, error) {
	var rows []Outbox
	if err := r.db.WithContext(ctx).Where("status = ?", outboxPending).Order("id").Limit(r.batch).Find(&rows).Error; err != nil {
		return 0, err
	}
	if len(rows) == 0 {
		return 0, nil
	}
	ids := make([]uint64, len(rows))
	msgs := make([]mq.OutMessage, len(rows))
	for i, row := range rows {
		ids[i] = row.ID
		msgs[i] = mq.OutMessage{
			Topic: row.Topic, Key: []byte(row.MsgKey), Value: []byte(row.Payload),
			Headers: map[string]string{"content-type": "application/json"},
		}
	}
	if err := r.pub.Publish(ctx, msgs...); err != nil {
		r.db.WithContext(ctx).Model(&Outbox{}).Where("id IN ?", ids).Update("attempts", gorm.Expr("attempts + 1"))
		return 0, err
	}
	now := r.now()
	if err := r.db.WithContext(ctx).Model(&Outbox{}).Where("id IN ?", ids).
		Updates(map[string]any{"status": outboxSent, "sent_at": now}).Error; err != nil {
		return 0, err
	}
	return len(rows), nil
}
