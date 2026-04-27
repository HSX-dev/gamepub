-- 钱包库 gamepub_wallet
-- 正确性约束全部落在数据库层：唯一索引兜底幂等，CHECK 兜底不出现负余额（MySQL 8.0.16+ 生效）。

CREATE TABLE IF NOT EXISTS wallet_account (
  id         BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  user_id    BIGINT UNSIGNED NOT NULL,
  available  BIGINT NOT NULL DEFAULT 0,
  frozen     BIGINT NOT NULL DEFAULT 0,
  version    BIGINT NOT NULL DEFAULT 0 COMMENT '乐观锁版本号，所有余额变更（含 TCC）都必须 +1',
  created_at DATETIME(3) NOT NULL,
  updated_at DATETIME(3) NOT NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uk_user (user_id),
  CONSTRAINT ck_wallet_non_negative CHECK (available >= 0 AND frozen >= 0)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS wallet_ledger (
  id            BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  user_id       BIGINT UNSIGNED NOT NULL,
  biz_type      VARCHAR(32) NOT NULL,
  biz_id        VARCHAR(64) NOT NULL,
  amount        BIGINT NOT NULL COMMENT '入账为正，扣减为负',
  balance_after BIGINT NOT NULL COMMENT '变更后总额（可用+冻结）',
  remark        VARCHAR(255) NULL,
  created_at    DATETIME(3) NOT NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uk_biz (biz_type, biz_id),
  KEY idx_user_created (user_id, created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS wallet_tcc_branch (
  id         BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  xid        VARCHAR(64) NOT NULL,
  user_id    BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '空回滚记录为 0',
  amount     BIGINT NOT NULL DEFAULT 0,
  status     VARCHAR(16) NOT NULL COMMENT 'tried / confirmed / cancelled',
  created_at DATETIME(3) NOT NULL,
  updated_at DATETIME(3) NOT NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uk_xid (xid)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS wallet_reconcile_diff (
  id             BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  user_id        BIGINT UNSIGNED NOT NULL,
  wallet_balance BIGINT NOT NULL,
  ledger_sum     BIGINT NOT NULL,
  checked_at     DATETIME(3) NOT NULL,
  PRIMARY KEY (id),
  KEY idx_user (user_id),
  KEY idx_checked_at (checked_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS mq_consumed (
  id         BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  consumer   VARCHAR(64)  NOT NULL,
  msg_id     VARCHAR(128) NOT NULL,
  created_at DATETIME(3)  NOT NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uk_consumer_msg (consumer, msg_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
