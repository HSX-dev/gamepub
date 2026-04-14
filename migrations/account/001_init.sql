-- 账户库 gamepub_account
-- 手机号不存明文：phone_hash 为带 pepper 的 HMAC 盲索引（唯一约束 + 登录查询），phone_enc 为 AES-GCM 密文。

CREATE TABLE IF NOT EXISTS account_user (
  id            BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  phone_hash    VARCHAR(64)  NOT NULL COMMENT 'HMAC-SHA256(pepper, phone)',
  phone_enc     VARCHAR(255) NOT NULL COMMENT 'AES-256-GCM 密文',
  phone_masked  VARCHAR(20)  NOT NULL,
  password_hash VARCHAR(100) NOT NULL COMMENT 'bcrypt',
  nickname      VARCHAR(64)  NOT NULL,
  avatar_key    VARCHAR(128) NULL,
  roles         VARCHAR(128) NOT NULL DEFAULT 'player',
  status        VARCHAR(16)  NOT NULL DEFAULT 'active',
  created_at    DATETIME(3)  NOT NULL,
  updated_at    DATETIME(3)  NOT NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uk_phone_hash (phone_hash)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
