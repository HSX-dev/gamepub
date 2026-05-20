-- 订单库 gamepub_order

CREATE TABLE IF NOT EXISTS order_order (
  id              BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  order_no        VARCHAR(32) NOT NULL,
  user_id         BIGINT UNSIGNED NOT NULL,
  idempotency_key VARCHAR(64) NULL COMMENT '客户端下单幂等键，按用户隔离',
  game_id         VARCHAR(32) NOT NULL,
  product_id      VARCHAR(32) NOT NULL,
  amount          BIGINT NOT NULL COMMENT '分，来自服务端商品目录',
  coin            BIGINT NOT NULL,
  channel         VARCHAR(16) NOT NULL,
  trade_no        VARCHAR(64) NULL COMMENT '渠道交易号，支付成功后写入',
  status          VARCHAR(16) NOT NULL,
  paid_at         DATETIME(3) NULL,
  created_at      DATETIME(3) NOT NULL,
  updated_at      DATETIME(3) NOT NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uk_order_no (order_no),
  -- NULL 不参与唯一约束：未带幂等键的订单、未支付订单互不冲突
  UNIQUE KEY uk_user_idem (user_id, idempotency_key),
  UNIQUE KEY uk_channel_trade (channel, trade_no),
  KEY idx_user_status (user_id, status),
  KEY idx_game_id (game_id),
  KEY idx_created_at (created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS order_game_app (
  game_id      VARCHAR(32)  NOT NULL,
  name         VARCHAR(64)  NOT NULL,
  delivery_url VARCHAR(512) NULL COMMENT '发货回调地址，保存与投递时均做 SSRF 校验',
  updated_by   VARCHAR(64)  NULL,
  created_at   DATETIME(3)  NOT NULL,
  updated_at   DATETIME(3)  NOT NULL,
  PRIMARY KEY (game_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS order_outbox (
  id         BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  topic      VARCHAR(128) NOT NULL,
  msg_key    VARCHAR(128) NULL,
  payload    TEXT NOT NULL,
  status     VARCHAR(16) NOT NULL COMMENT 'pending / sent',
  attempts   INT NOT NULL DEFAULT 0,
  created_at DATETIME(3) NOT NULL,
  sent_at    DATETIME(3) NULL,
  PRIMARY KEY (id),
  KEY idx_status_id (status, id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS mq_consumed (
  id         BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  consumer   VARCHAR(64)  NOT NULL,
  msg_id     VARCHAR(128) NOT NULL,
  created_at DATETIME(3)  NOT NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uk_consumer_msg (consumer, msg_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
