#!/bin/sh
# 本地开发：每个服务独立数据库 + 独立账号，账号只授权自己的库（最小权限，一个服务被攻破不能读写其它服务的数据）。
set -eu

mysql -uroot -p"${MYSQL_ROOT_PASSWORD}" <<SQL
CREATE DATABASE IF NOT EXISTS gamepub_account CHARACTER SET utf8mb4;
CREATE DATABASE IF NOT EXISTS gamepub_wallet  CHARACTER SET utf8mb4;
CREATE DATABASE IF NOT EXISTS gamepub_order   CHARACTER SET utf8mb4;
CREATE DATABASE IF NOT EXISTS gamepub_task    CHARACTER SET utf8mb4;

CREATE USER IF NOT EXISTS 'account'@'%' IDENTIFIED BY '${ACCOUNT_DB_PASSWORD}';
CREATE USER IF NOT EXISTS 'wallet'@'%'  IDENTIFIED BY '${WALLET_DB_PASSWORD}';
CREATE USER IF NOT EXISTS 'order'@'%'   IDENTIFIED BY '${ORDER_DB_PASSWORD}';
CREATE USER IF NOT EXISTS 'task'@'%'    IDENTIFIED BY '${TASK_DB_PASSWORD}';

-- 开发环境开启 AUTO_MIGRATE 需要 DDL 权限；生产账号只授予 SELECT/INSERT/UPDATE/DELETE，DDL 由迁移账号执行
GRANT ALL PRIVILEGES ON gamepub_account.* TO 'account'@'%';
GRANT ALL PRIVILEGES ON gamepub_wallet.*  TO 'wallet'@'%';
GRANT ALL PRIVILEGES ON gamepub_order.*   TO 'order'@'%';
GRANT ALL PRIVILEGES ON gamepub_task.*    TO 'task'@'%';
FLUSH PRIVILEGES;
SQL
