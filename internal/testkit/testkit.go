// Package testkit 测试基础设施：内存 SQLite（纯 Go，无需 CGO）与 miniredis。
package testkit

import (
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/glebarez/sqlite"
	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"

	"gamepub/pkg/dbx"
)

var seq atomic.Int64

// NewDB 创建独立的内存数据库并迁移给定模型。
// SQLite 单连接串行执行，用于验证业务逻辑；真实并发行为（行锁、间隙锁）需在 MySQL 集成测试中覆盖。
func NewDB(t testing.TB, models ...any) *gorm.DB {
	t.Helper()
	name := strings.NewReplacer("/", "_", " ", "_").Replace(t.Name())
	dsn := fmt.Sprintf("file:%s_%d?mode=memory&cache=shared&_pragma=busy_timeout(5000)", name, seq.Add(1))
	db, err := dbx.Open(sqlite.Open(dsn), dbx.PoolConfig{MaxOpen: 1, MaxIdle: 1}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(models...); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dbx.Close(db) })
	return db
}

// NewRedis 启动 miniredis 并返回客户端。
func NewRedis(t testing.TB) (*miniredis.Miniredis, *redis.Client) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return mr, rdb
}
