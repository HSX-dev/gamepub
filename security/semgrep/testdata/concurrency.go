package testdata

import (
	"context"
	"database/sql"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"github.com/segmentio/kafka-go"
)

type svc struct{ rdb *redis.Client }

func (s *svc) Charge(ctx context.Context, uid uint64) error {
	// ruleid: context-background-in-request-path
	cctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = cctx
	// ok: context-background-in-request-path
	bg := context.WithoutCancel(ctx)
	_ = bg
	return nil
}

func (s *svc) handler(c *gin.Context) {
	// ruleid: context-background-in-request-path
	_ = context.TODO()
}

func main() {
	// ok: context-background-in-request-path
	_ = context.Background()
}

func scan(db *sql.DB, ids []int) {
	for _, id := range ids {
		rows, _ := db.Query("SELECT 1 WHERE id = ?", id)
		// ruleid: defer-in-loop
		defer rows.Close()
	}
	for _, id := range ids {
		func() {
			rows, _ := db.Query("SELECT 1 WHERE id = ?", id)
			// ok: defer-in-loop
			defer rows.Close()
		}()
	}
}

func (s *svc) unlock(ctx context.Context) {
	// ruleid: redis-lock-unsafe-release
	s.rdb.Del(ctx, "wallet:reconcile:lock")
	// ok: redis-lock-unsafe-release
	s.rdb.Del(ctx, "account:profile:1")
}

func (s *svc) lock(ctx context.Context) {
	// ruleid: redis-setnx-then-expire
	s.rdb.SetNX(ctx, "job", "1", 0)
	s.rdb.Expire(ctx, "job", time.Minute)
}

func consume(ctx context.Context, r *kafka.Reader) {
	// ruleid: kafka-readmessage-autocommit
	m, _ := r.ReadMessage(ctx)
	// ok: kafka-readmessage-autocommit
	m2, _ := r.FetchMessage(ctx)
	_, _ = m, m2
}
