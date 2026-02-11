// Package dbx 封装 GORM 初始化：连接池、慢查询日志（不打印参数值）、唯一键冲突识别。
package dbx

import (
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	mysqldrv "github.com/go-sql-driver/mysql"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// PoolConfig 连接池参数。连接数需结合 MySQL max_connections 与 Pod 副本数评估，避免扩容时打满数据库。
type PoolConfig struct {
	MaxOpen     int
	MaxIdle     int
	MaxLifetime time.Duration
	MaxIdleTime time.Duration
}

// DefaultPool 单 Pod 默认连接池。
func DefaultPool() PoolConfig {
	return PoolConfig{MaxOpen: 50, MaxIdle: 10, MaxLifetime: 30 * time.Minute, MaxIdleTime: 5 * time.Minute}
}

// Open 用指定方言打开数据库。
func Open(dialector gorm.Dialector, pool PoolConfig, slowThreshold time.Duration) (*gorm.DB, error) {
	db, err := gorm.Open(dialector, &gorm.Config{
		// 统一把各驱动的唯一键冲突翻译为 gorm.ErrDuplicatedKey，幂等逻辑依赖它
		TranslateError: true,
		// 关闭 GORM 对单条写操作的隐式事务；需要事务的地方显式 Transaction
		SkipDefaultTransaction: true,
		Logger: logger.New(log.New(os.Stdout, "", log.LstdFlags), logger.Config{
			SlowThreshold:             slowThreshold,
			LogLevel:                  logger.Warn,
			IgnoreRecordNotFoundError: true,
			// 慢 SQL 日志只打印占位符，不打印参数值（手机号、金额等敏感数据）
			ParameterizedQueries: true,
		}),
	})
	if err != nil {
		return nil, fmt.Errorf("dbx: open: %w", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		return nil, fmt.Errorf("dbx: underlying db: %w", err)
	}
	sqlDB.SetMaxOpenConns(pool.MaxOpen)
	sqlDB.SetMaxIdleConns(pool.MaxIdle)
	sqlDB.SetConnMaxLifetime(pool.MaxLifetime)
	sqlDB.SetConnMaxIdleTime(pool.MaxIdleTime)
	return db, nil
}

// OpenMySQL DSN 必须来自 Secret，且建议带上 parseTime=true&loc=UTC&timeout=3s&readTimeout=5s&writeTimeout=5s。
func OpenMySQL(dsn string, pool PoolConfig) (*gorm.DB, error) {
	return Open(mysql.Open(dsn), pool, 200*time.Millisecond)
}

// Close 关闭底层连接池。
func Close(db *gorm.DB) error {
	sqlDB, err := db.DB()
	if err != nil {
		return err
	}
	return sqlDB.Close()
}

// IsDuplicate 判断是否唯一键冲突（MySQL 1062 / SQLite UNIQUE）。
func IsDuplicate(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		return true
	}
	var me *mysqldrv.MySQLError
	if errors.As(err, &me) && me.Number == 1062 {
		return true
	}
	return strings.Contains(err.Error(), "UNIQUE constraint failed")
}
