-- 任务库 gamepub_task

CREATE TABLE IF NOT EXISTS task_user_task (
  id         BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  user_id    BIGINT UNSIGNED NOT NULL,
  task_id    VARCHAR(32) NOT NULL,
  period     VARCHAR(16) NOT NULL COMMENT '20261015 / 2026W42 / once',
  progress   BIGINT NOT NULL DEFAULT 0,
  target     BIGINT NOT NULL,
  status     VARCHAR(16) NOT NULL COMMENT 'in_progress / completed / claiming / claimed',
  created_at DATETIME(3) NOT NULL,
  updated_at DATETIME(3) NOT NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uk_user_task_period (user_id, task_id, period),
  KEY idx_status (status),
  KEY idx_updated_at (updated_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS mq_consumed (
  id         BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  consumer   VARCHAR(64)  NOT NULL,
  msg_id     VARCHAR(128) NOT NULL,
  created_at DATETIME(3)  NOT NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uk_consumer_msg (consumer, msg_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
