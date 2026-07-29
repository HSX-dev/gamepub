# 代码安全审计报告（样例）

> 本报告为样例，对应某一历史版本（存在漏洞）的审计过程与修复验证。文中“问题代码”是该版本的代码，“修复后代码”摘自当前仓库。报告格式即本团队实际使用的格式。

## 审计信息

| 项目 | 内容 |
|---|---|
| 系统 | 游戏发行平台核心后端（账户 · 资金 · 订单 · 任务 · 权限） |
| 审计范围 | `cmd/*`、`internal/*`、`pkg/*` 全部一方代码；`api/gen`（生成代码）与第三方依赖不在人工审查范围，依赖问题由 govulncheck / Trivy 覆盖 |
| 代码版本 | 审计基线 commit（脱敏），修复后版本即当前主干 |
| 审计时间 | 2026-09-01 至 2026-09-18 |
| 方法 | 人工代码审查为主；工具辅助：Semgrep 自定义规则、gosec、`go vet`、`go test -race`、govulncheck、Trivy；AI 辅助初筛（结果全部人工复核，不自动阻断、不自动关闭，见 [risk-rating.md](risk-rating.md) 第 5 节） |
| 分级标准 | [risk-rating.md](risk-rating.md) |
| 复审 | 每条修复需有回归测试并通过人工复审（见 [risk-rating.md](risk-rating.md) 第 4 节） |

## 统计

| 等级 | 数量 | 编号 |
|---|---|---|
| 严重 | 1 | GP-AUD-006 |
| 高 | 6 | GP-AUD-001、002、004、007、010、012 |
| 中 | 5 | GP-AUD-003、005、008、009、011 |
| 低 | 0 | — |
| 不成立 / 已抑制 | 1 | GP-AUD-013 |

截至 2026-10-10，12 个成立问题全部“已关闭”（有回归测试并通过复审），1 个经人工复核判定不成立。遗留的非阻断建议见文末。

关于 CVSS 与最终等级：本平台最终等级取“CVSS 等级”与“业务下限”中较高者（见 [risk-rating.md](risk-rating.md) 第 1.1 节）。资金类与可批量越权类问题多因此被上调，报告中逐条注明。

---

## GP-AUD-001 按订单号查询可读取他人订单（水平越权）

| | |
|---|---|
| 等级 | 高 |
| CWE | CWE-639 Authorization Bypass Through User-Controlled Key |
| CVSS v3.1 | `AV:N/AC:L/PR:L/UI:N/S:U/C:H/I:N/A:N` = 6.5（中）；按业务下限（可批量越权读取他人数据）上调为**高** |
| 位置 | [internal/order/service.go](../../internal/order/service.go) `GetOrder` / `BatchGetOrders` |
| 来源 | 人工审查 + Semgrep 自定义规则 `order-lookup-without-owner` |

**描述**：订单查询只用 `order_no` 作为过滤条件，没有校验订单归属。任何登录用户传入他人的订单号即可读取对方订单。叠加旧版订单号为“时间戳 + 自增序号”的可预测格式，攻击者可批量枚举。

**影响**：越权读取全体用户的订单（商品、金额、下单时间，并可关联 user_id），属于可批量利用的水平越权与信息泄露。

**复现（针对漏洞版本）**：

```bash
# 玩家 A 的令牌，查询属于玩家 B 的订单号
curl -s https://api.example.com/api/v1/orders/20260115093000000042 \
  -H "Authorization: Bearer <玩家A的token>"
# 漏洞版本返回玩家 B 的订单详情
```

**问题代码**：

```go
func (s *Service) GetOrder(ctx context.Context, userID uint64, orderNo string) (*Order, error) {
	var o Order
	// 只按订单号查询，未校验归属
	err := s.db.WithContext(ctx).Where("order_no = ?", orderNo).Take(&o).Error
	return &o, err
}
```

**修复方案**：查询条件同时包含 `order_no` 与 `user_id`；他人订单与不存在的订单返回同一个 `ErrOrderNotFound`，既防越权也不泄露订单号是否存在。批量接口对调用方的订单做服务端过滤。订单号改为“时间戳 + 48 位密码学随机数”，但归属校验才是真正的防线，不依赖订单号不可预测。

**修复后代码**：

```go
func (s *Service) GetOrder(ctx context.Context, userID uint64, orderNo string) (*Order, error) {
	if userID == 0 || !orderNoPattern.MatchString(orderNo) {
		return nil, ErrOrderNotFound
	}
	var o Order
	err := s.db.WithContext(ctx).Where("order_no = ? AND user_id = ?", orderNo, userID).Take(&o).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrOrderNotFound
	}
	return &o, err
}
```

网关侧 `user_id` 只取自令牌（[handler_trade.go](../../internal/gateway/handler_trade.go) `getOrder` 使用 `userID(c)`），不读 query/body/path。

**回归测试**：[internal/order/service_test.go](../../internal/order/service_test.go) `TestHorizontalPrivilegeEscalationBlocked`（他人订单返回 NotFound，批量只返回本人订单）；[internal/gateway/router_test.go](../../internal/gateway/router_test.go) `TestUserIDAlwaysComesFromToken`。

**复审结论**：已关闭（2026-09-10）。根因修复（查询加归属，非事后比较）；同类排查：Semgrep 规则扫描全仓库 `Where("... = ?"` 单条件查询，确认 `ListOrders`、`BatchGetOrders`、`ListLedger` 均已带 `user_id`，仅支付回调为设计例外（见 GP-AUD-013）。

---

## GP-AUD-002 后台接口缺失鉴权中间件

| | |
|---|---|
| 等级 | 高 |
| CWE | CWE-862 Missing Authorization |
| CVSS v3.1 | `AV:N/AC:L/PR:N/UI:N/S:U/C:H/I:N/A:N` = 7.5（高） |
| 位置 | [internal/gateway/router.go](../../internal/gateway/router.go) 路由注册 |
| 来源 | 人工审查（由 `TestEveryNonPublicRouteRequiresAuth` 触发） |

**描述**：旧版把后台订单查询路由直接注册在根引擎上，漏挂了鉴权与权限中间件，导致未登录即可访问后台订单列表（可按 user_id 过滤查询任意用户订单）。问题的根源不是“这一个路由写错了”，而是“鉴权靠开发者逐个记得加”。

**影响**：未授权读取全体用户订单。若同样的疏漏发生在 `wallet/adjust` 路由上，则是未授权任意调账，等级为严重。

**复现（针对漏洞版本）**：

```bash
curl -s "https://api.example.com/admin/v1/orders?user_id=42" # 无 Authorization 头即返回数据
```

**问题代码**：

```go
admin := r.Group("/admin/v1", authn(d.Verifier), loadStaff(d.Account), auditLog(d.Logger))
admin.PUT("/games/:game_id", requirePerm(rbac.PermGameWrite), h.adminUpsertGame)
// 新增时误注册到根引擎 r，而非 admin 组，绕过 authn/loadStaff/requirePerm
r.GET("/admin/v1/orders", h.adminOrders)
```

**修复方案**：所有后台路由挂在 `admin` 分组（`authn → loadStaff → auditLog`）上，每个路由再挂 `requirePerm`。公开接口以白名单 `publicRoutes` 显式列出。用全路由遍历测试把“漏挂中间件”变成 CI 失败，而不是靠人记。

**修复后代码**：

```go
admin := r.Group("/admin/v1", bodyLimit(64<<10), authn(d.Verifier), loadStaff(d.Account), auditLog(d.Logger))
admin.GET("/orders", requirePerm(rbac.PermOrderRead), h.adminOrders)
admin.PUT("/games/:game_id", requirePerm(rbac.PermGameWrite), h.adminUpsertGame)
admin.POST("/wallet/adjust", requirePerm(rbac.PermWalletAdjust), h.adminAdjustWallet)
admin.GET("/reconcile/diffs", requirePerm(rbac.PermWalletRead), h.adminReconcileDiffs)
```

**回归测试**：[router_test.go](../../internal/gateway/router_test.go) `TestEveryNonPublicRouteRequiresAuth`（遍历全部路由，白名单外无令牌必须 401）、`TestAdminRoutesRequireStaffAndPermission`（玩家与停用员工必须 403；令牌里的角色不被信任）。

**复审结论**：已关闭（2026-09-08）。根因修复为“分组 + 全路由回归网”。确认 `TestEveryNonPublicRouteRequiresAuth` 会因路由数下降或漏挂而失败（回退修复后测试转红）。

---

## GP-AUD-003 批量查询接口无上限、无限流

| | |
|---|---|
| 等级 | 中 |
| CWE | CWE-770 Allocation of Resources Without Limits or Throttling |
| CVSS v3.1 | `AV:N/AC:L/PR:L/UI:N/S:U/C:L/I:N/A:L` = 5.4（中） |
| 位置 | [internal/order/service.go](../../internal/order/service.go) `BatchGetOrders`、[internal/gateway/router.go](../../internal/gateway/router.go)、[internal/gateway/handler_trade.go](../../internal/gateway/handler_trade.go) |
| 来源 | 人工审查 |

**描述**：批量查询订单接口对 `order_nos` 数组长度无上限，也没有独立限流。它既是数据库压力放大器（单请求触发超大 `IN` 查询），也是越权枚举的放大器（配合 GP-AUD-001）。

**影响**：构造超大数组可拖慢数据库；高频调用可放大枚举速度。属于局部拒绝服务与枚举加速。

**复现（针对漏洞版本）**：

```bash
curl -s https://api.example.com/api/v1/orders/batch-query \
  -H "Authorization: Bearer <token>" -H "Content-Type: application/json" \
  -d '{"order_nos": ["X", "X", ... 共 100000 个 ...]}'
```

**问题代码**：

```go
func (s *Service) BatchGetOrders(ctx context.Context, userID uint64, orderNos []string) ([]Order, error) {
	var rows []Order
	err := s.db.WithContext(ctx).Where("order_no IN ?", orderNos).Find(&rows).Error // 无上限、无归属
	return rows, err
}
```

**修复方案**：三层限制——请求体字段校验（`binding:"max=50,dive,max=32"`）、服务层 `MaxBatch` 二次校验、网关对该接口单独挂更严格的限流器；并与 GP-AUD-001 一起加上归属过滤和订单号格式校验、去重。

**修复后代码**：

```go
// service.go
const MaxBatch = 50

func (s *Service) BatchGetOrders(ctx context.Context, userID uint64, orderNos []string) ([]Order, error) {
	if userID == 0 {
		return nil, ErrInvalidArgument
	}
	if len(orderNos) > MaxBatch {
		return nil, ErrTooManyOrders
	}
	// 去重 + 格式校验后，只查本人订单
	// ...
	err := s.db.WithContext(ctx).Where("user_id = ? AND order_no IN ?", userID, nos).Order("id DESC").Find(&rows).Error
	return rows, err
}
```

```go
// router.go：批量接口单独限流（配额更小）
api.POST("/orders/batch-query", rateLimit(d.Limiters.Batch, byUser), h.batchOrders)
// handler_trade.go
type batchOrdersReq struct {
	OrderNos []string `json:"order_nos" binding:"required,min=1,max=50,dive,max=32"`
}
```

生产限流配额见 [cmd/gateway/main.go](../../cmd/gateway/main.go)：`Batch` 为 `Rate:1, Burst:3`。

**回归测试**：[router_test.go](../../internal/gateway/router_test.go) `TestBatchQueryLimit`（51 条返回 400）；[service_test.go](../../internal/order/service_test.go) `TestHorizontalPrivilegeEscalationBlocked`（`MaxBatch+1` 返回 `ErrTooManyOrders`）。

**复审结论**：已关闭（2026-09-11）。同类排查：确认 `ListOrders`、`ListLedger`、`AdminListOrders` 均经 `sqlsafe.Page` 限制页大小与深分页。

---

## GP-AUD-004 后台排序参数 SQL 注入

| | |
|---|---|
| 等级 | 高 |
| CWE | CWE-89 SQL Injection |
| CVSS v3.1 | `AV:N/AC:L/PR:L/UI:N/S:U/C:H/I:N/A:L` = 7.1（高） |
| 位置 | [internal/order/service.go](../../internal/order/service.go) `AdminListOrders`、[internal/wallet/service.go](../../internal/wallet/service.go) `ListLedger`、[pkg/sqlsafe/sqlsafe.go](../../pkg/sqlsafe/sqlsafe.go) |
| 来源 | Semgrep（`gorm` 动态 `Order` 规则）+ 人工确认 |

**描述**：后台订单列表的排序字段直接拼进 `db.Order(...)`。参数化查询只能保护“值”，保护不了排序字段这类标识符，因此 `ORDER BY` 位置存在 SQL 注入。拥有 `order:read` 的客服账号即可利用。

**影响**：通过 `ORDER BY` 的布尔 / 时间盲注可提取库中任意数据（含手机号密文、口令哈希）。go-sql-driver 默认禁用多语句，`; DROP TABLE` 这类堆叠语句无法直接执行，但盲注读取与重查询造成的负载仍然成立，故定 C:H、A:L。

**复现（针对漏洞版本）**：

```bash
# 时间盲注：条件为真时 SLEEP(3)
curl -s "https://api.example.com/admin/v1/orders?sort_by=(CASE+WHEN+(SELECT+1)=1+THEN+SLEEP(3)+ELSE+id+END)" \
  -H "Authorization: Bearer <cs token>"
```

**问题代码**：

```go
func (s *Service) AdminListOrders(ctx context.Context, q AdminQuery) ([]Order, int64, error) {
	db := s.db.WithContext(ctx).Model(&Order{})
	order := q.SortBy
	if q.Desc {
		order += " DESC"
	}
	// q.SortBy 来自请求，直接拼进 ORDER BY
	err := db.Order(order).Offset(...).Limit(...).Find(&rows).Error
	// ...
}
```

**修复方案**：所有动态排序字段经白名单映射为代码内常量列名，用 `clause.OrderByColumn` 构造（方言会正确转义），非法字段回落默认列或直接报参数错误。过滤条件（status、game_id、biz_type）也走白名单或正则。

**修复后代码**：

```go
// sqlsafe.go
func (f SortFields) OrderBy(field string, desc bool, fallback string) clause.OrderByColumn {
	col, ok := f[field]
	if !ok {
		col = fallback
	}
	return clause.OrderByColumn{Column: clause.Column{Name: col}, Desc: desc}
}

// order/service.go
var adminSort = sqlsafe.SortFields{"created_at": "created_at", "amount": "amount", "paid_at": "paid_at", "id": "id"}

func (s *Service) AdminListOrders(ctx context.Context, q AdminQuery) ([]Order, int64, error) {
	// ...
	if _, err := adminSort.Resolve(q.SortBy); err != nil {
		return nil, 0, ErrInvalidArgument
	}
	err := db.Order(adminSort.OrderBy(q.SortBy, q.Desc, "id")).Offset(offset).Limit(limit).Find(&rows).Error
	// ...
}
```

**回归测试**：[service_test.go](../../internal/order/service_test.go) `TestAdminListOrdersFilters`（注入载荷返回 `ErrInvalidArgument`，表行数不变）；[internal/wallet/service_test.go](../../internal/wallet/service_test.go) `TestListLedgerSortWhitelist`（注入载荷安全回落默认排序）；[pkg/sqlsafe/sqlsafe_test.go](../../pkg/sqlsafe/sqlsafe_test.go) `TestOrderByWhitelist`。

**复审结论**：已关闭（2026-09-12）。补充 Semgrep 规则覆盖 `db.Order(`、`gorm.Expr(` 拼接。同类排查：全仓库动态排序只在 order 与 wallet 两处，均已改为白名单。

---

## GP-AUD-005 头像上传仅校验文件名后缀

| | |
|---|---|
| 等级 | 中 |
| CWE | CWE-434 Unrestricted Upload of File with Dangerous Type |
| CVSS v3.1 | `AV:N/AC:L/PR:L/UI:R/S:C/C:L/I:L/A:N` = 5.2（中） |
| 位置 | [pkg/upload/upload.go](../../pkg/upload/upload.go)、[internal/gateway/handler_user.go](../../internal/gateway/handler_user.go) `uploadAvatar` |
| 来源 | 人工审查 |

**描述**：旧版头像上传只看文件名后缀，并用用户提交的文件名落盘。可上传内容为 `<svg><script>` 或 `<?php ...?>` 的 `.png`，造成存储型 XSS / 潜在脚本执行；用户文件名含 `../` 可路径穿越。

**影响**：若头像以可内联渲染的方式被其他用户或运营后台加载，触发存储型 XSS（作用域变更，S:C）；配合可解析执行的存储目录可导致服务端代码执行；路径穿越可覆盖任意文件。

**复现（针对漏洞版本）**：

```bash
# 内容是 HTML/JS，仅文件名后缀为 .png
printf '<html><script>alert(document.cookie)</script></html>' > x.png
curl -s https://api.example.com/api/v1/me/avatar \
  -H "Authorization: Bearer <token>" -F "file=@x.png"
# 路径穿越
curl -s https://api.example.com/api/v1/me/avatar \
  -H "Authorization: Bearer <token>" -F "file=@shell.png;filename=../../../etc/cron.d/evil"
```

**问题代码**：

```go
func (h *handlers) uploadAvatar(c *gin.Context) {
	fh, _ := c.FormFile("file")
	if !strings.HasSuffix(fh.Filename, ".png") && !strings.HasSuffix(fh.Filename, ".jpg") {
		badRequest(c, "only png/jpg")
		return
	}
	dst := filepath.Join(h.uploadDir, fh.Filename) // 用用户文件名
	_ = c.SaveUploadedFile(fh, dst)
	ok(c, gin.H{"avatar_key": fh.Filename})
}
```

**修复方案**：按文件头魔数判断类型并与 `DecodeConfig` 的格式交叉验证；解码前检查尺寸（防像素炸弹）；完整解码后重新编码（剥离 EXIF 与附加载荷）；存储 key 由服务端随机生成（`avatars/{uid}/<random>.png`），本地存储再用 `filepath.Rel` 确认不越界。

**修复后代码**：

```go
// upload.go Inspect（节选）
ct := http.DetectContentType(data)
ext, ok := allowedImages[ct]
if !ok {
	return nil, ErrType
}
cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
if err != nil || "image/"+format != ct {
	return nil, ErrType
}
if cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width > p.MaxWidth || cfg.Height > p.MaxHeight {
	return nil, ErrDimensions
}
img, _, err := image.Decode(bytes.NewReader(data)) // 完整解码后重新编码
// ...

// handler_user.go
img, err := upload.AvatarPolicy().Inspect(f)
key := upload.NewKey(fmt.Sprintf("avatars/%d", uid), img.Ext) // 服务端生成 key
```

账户服务另有 `UpdateProfile` 校验 `avatar_key` 必须位于 `avatars/{uid}/` 下（[account/service.go](../../internal/account/service.go)），防止引用他人目录。

**回归测试**：[router_test.go](../../internal/gateway/router_test.go) `TestAvatarUpload`（HTML 伪装 png 返回 400；超大返回 413；恶意文件名被忽略，服务端生成 key）；[pkg/upload/upload_test.go](../../pkg/upload/upload_test.go) `TestInspect`（像素炸弹、trailing payload、svg）、`TestLocalStorageRejectsTraversal`；[account/service_test.go](../../internal/account/service_test.go) `TestAvatarMustBelongToUser`。

**复审结论**：已关闭（2026-09-13）。补充运维确认：对象存储为私有桶 + 签名 URL，头像不在可执行目录。

---

## GP-AUD-006 支付回调未验签、可重放、未校验金额

| | |
|---|---|
| 等级 | 严重 |
| CWE | CWE-345 Insufficient Verification of Data Authenticity；CWE-294 Authentication Bypass by Capture-replay |
| CVSS v3.1 | `AV:N/AC:L/PR:N/UI:N/S:U/C:N/I:H/A:N` = 7.5（高）；按业务下限（无条件直接造成资金损失）上调为**严重** |
| 位置 | [internal/order/callback.go](../../internal/order/callback.go)、[pkg/signature/signature.go](../../pkg/signature/signature.go)、[internal/gateway/handler_trade.go](../../internal/gateway/handler_trade.go) `payNotify` |
| 来源 | 人工审查 |

**描述**：旧版支付回调直接信任渠道报文：不验签、无防重放、不校验金额。攻击者可直接伪造一条 `status=SUCCESS` 的通知把自己的订单置为已支付，或截获一条真实通知反复投递。

**影响**：免费充值到账，直接资金损失，且无需任何前置条件。这是全系统最高优先级问题。

**复现（针对漏洞版本）**：

```bash
# 自己下一笔 pending 订单，然后伪造回调，金额随意
curl -s https://api.example.com/api/v1/pay/notify/mockpay \
  -H "Content-Type: application/json" \
  -d '{"order_no":"<自己的订单号>","trade_no":"FAKE1","amount":1,"status":"SUCCESS"}'
# 漏洞版本应答 success，订单变为 paid，随后充值到账
```

**问题代码**：

```go
func (s *Service) HandlePayNotify(ctx context.Context, channel string, body []byte, headers map[string]string) (string, error) {
	var n payNotify
	_ = json.Unmarshal(body, &n)
	// 不验签、不防重放、不校验金额与渠道
	s.db.WithContext(ctx).Model(&Order{}).
		Where("order_no = ?", n.OrderNo).
		Updates(map[string]any{"status": StatusPaid})
	return "success", nil
}
```

**修复方案**：

1. 网关原样透传 body 与三个签名头，不解析、不持有渠道密钥；
2. 对原始报文做 HMAC 验签（`hmac.Equal` 常量时间比较）+ 时间窗 + nonce 防重放；验签通过后才占用 nonce；nonce 存储不可用时拒绝（fail closed）；
3. 业务校验：渠道白名单、订单存在、渠道一致、金额一致；
4. 状态机 CAS（`WHERE status='pending'`）+ 渠道交易号唯一（`uk_channel_trade`）+ outbox 同事务写入；
5. 业务步骤失败时释放 nonce，使渠道用原报文重试不被当成重放。

**修复后代码**：

```go
// callback.go
func (s *Service) HandlePayNotify(ctx context.Context, channel string, body []byte, headers map[string]string) (string, error) {
	v, ok := s.verifiers[channel]
	if !ok {
		return "", ErrChannelNotSupported
	}
	// ... 取小写头 ...
	if err := v.Verify(ctx, ts, nonce, sig, body); err != nil {
		return "", ErrBadSignature
	}
	reply, err := s.applyPayNotify(ctx, channel, body)
	if err != nil {
		_ = v.Release(ctx, nonce) // 业务失败释放 nonce，允许渠道原报文重试
		return "", err
	}
	return reply, nil
}

// applyPayNotify（节选）：金额校验 + 状态 CAS + outbox 同事务
if o.Amount != n.Amount {
	s.logger.ErrorContext(ctx, "pay notify amount mismatch", "order_no", o.OrderNo, ...)
	return ErrAmountMismatch
}
r := tx.Model(&Order{}).Where("id = ? AND status = ?", o.ID, StatusPending).
	Updates(map[string]any{"status": StatusPaid, "trade_no": &tradeNo, "paid_at": paidAt})
if r.RowsAffected == 0 {
	return errConcurrentPaid
}
return tx.Create(&Outbox{Topic: events.TopicOrderPaid, ...}).Error
```

```go
// signature.go：常量时间比较 + 验签通过后才占用 nonce + fail closed
got, err := hex.DecodeString(sig)
if err != nil || !hmac.Equal(got, mac(v.secret, timestamp, nonce, body)) {
	return ErrInvalid
}
first, err := v.store.Claim(ctx, v.scope+":"+nonce, 2*v.window)
if err != nil {
	return fmt.Errorf("signature: nonce store unavailable: %w", err) // 拒绝
}
if !first {
	return ErrReplay
}
```

**回归测试**：[internal/order/service_test.go](../../internal/order/service_test.go) `TestPayNotifyHappyPathAndIdempotency`（重复通知幂等、原报文重放被拒）、`TestPayNotifyRejections`（金额篡改拒绝、失败后同 nonce 重试放行、渠道交易号复用拒绝、错误签名拒绝）；[pkg/signature/signature_test.go](../../pkg/signature/signature_test.go) `TestVerify`、`TestForgedRequestDoesNotBurnNonce`、`TestNonceStoreFailureFailsClosed`；[router_test.go](../../internal/gateway/router_test.go) `TestPayNotifyForwardsSignatureHeaders`。

**复审结论**：已关闭（2026-09-07，作为严重问题优先修复并最先验收）。确认：金额以订单表为准；已支付订单被不同交易号二次支付时记 ERROR 并提示需退款（不静默覆盖）；临时缓解（上线前先在网关对该路径加来源 IP 白名单）在根因修复上线后撤除。

---

## GP-AUD-007 发货 Webhook 配置 SSRF（含 DNS rebinding 与重定向绕过）

| | |
|---|---|
| 等级 | 高 |
| CWE | CWE-918 Server-Side Request Forgery |
| CVSS v3.1 | `AV:N/AC:L/PR:H/UI:N/S:C/C:H/I:N/A:N` = 6.8（中）；按业务下限（可获取服务端凭据 / 访问内网）上调为**高** |
| 位置 | [pkg/netguard/netguard.go](../../pkg/netguard/netguard.go)、[internal/order/service.go](../../internal/order/service.go) `UpsertGameApp` / `TestDelivery`、[internal/order/delivery.go](../../internal/order/delivery.go) |
| 来源 | 人工审查 |

**描述**：游戏发货地址由运营配置、由平台服务端发起请求，是典型 SSRF 入口。旧版只在保存时用字符串规则校验（甚至只在保存时才校验），存在三类绕过：(1) 仅校验保存时解析结果，DNS rebinding 在真正请求时解析到内网；(2) 302 重定向到内网；(3) 探测接口回显状态码可做内网端口扫描。

**影响**：以平台服务端身份访问内网服务、读取云厂商元数据（如 `169.254.169.254`、CGNAT 段的 `100.100.100.200`）获取临时凭据，作用域变更。需要 `game:write` 权限（运营），故 PR:H，但一旦利用影响严重，按业务下限定为高。

**复现（针对漏洞版本）**：

```bash
curl -s https://api.example.com/admin/v1/games/demo_game \
  -H "Authorization: Bearer <ops token>" -H "Content-Type: application/json" \
  -X PUT -d '{"name":"Demo","delivery_url":"http://169.254.169.254/latest/meta-data/iam/security-credentials/"}'
# 或配置一个受控域名，保存时解析到公网 IP、发货时 rebind 到 127.0.0.1
```

**问题代码**：

```go
func (s *Service) UpsertGameApp(ctx context.Context, gameID, name, deliveryURL, operator string) (*GameApp, error) {
	if deliveryURL != "" {
		u, _ := url.Parse(deliveryURL)
		if strings.Contains(u.Host, "127.0.0.1") || strings.HasPrefix(u.Host, "10.") { // 黑名单字符串匹配
			return nil, ErrInvalidDeliveryURL
		}
	}
	// 投递时直接用 http.DefaultClient.Post(deliveryURL, ...)，不再校验、跟随重定向
}
```

**修复方案**：两道防线。(1) `ValidateURL` 在保存时校验协议、无凭据、端口白名单，并要求解析出的**所有** IP 均为公网（含回环、私网、链路本地、CGNAT、IPv4-mapped、NAT64、6to4）；(2) 真正请求时用 `Guard.HTTPClient`，在 `Dialer.Control` 中对最终 IP 二次校验，禁用代理、限制重定向次数并校验重定向协议——rebinding 与重定向都在建连前被拦下。投递前再次 `ValidateURL`（库中地址可能保存于规则收紧前）。探测接口统一返回状态码，不可达返回 0，不回显细节。

**修复后代码**：

```go
// netguard.go：建连前校验最终 IP
func (g *Guard) Dialer(timeout time.Duration) *net.Dialer {
	return &net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second,
		Control: func(network, address string, _ syscall.RawConn) error {
			ap, err := netip.ParseAddrPort(address)
			if err != nil {
				return blocked("unparseable address")
			}
			if !g.allow(netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port())) {
				return blocked("address not public or port not allowed")
			}
			return nil
		}}
}
// HTTPClient：Proxy 置空 + 限制重定向 + 校验重定向协议

// order/delivery.go：投递前再次校验库中地址
if _, err := s.guard.ValidateURL(ctx, app.DeliveryURL); err != nil {
	return mq.Permanent(fmt.Errorf("%w: %v", ErrInvalidDeliveryURL, err))
}
```

**回归测试**：[internal/order/service_test.go](../../internal/order/service_test.go) `TestGameAppDeliveryURLSSRF`（回环、元数据、私网、`file://`、带凭据 URL 均被拒）；[pkg/netguard/netguard_test.go](../../pkg/netguard/netguard_test.go) `TestIsPublic`、`TestValidateURL`（多解析结果任一不合规即拒、nxdomain）、`TestHTTPClientBlocksLoopbackAtDialTime`、`TestRedirectToInternalBlocked`。

**复审结论**：已关闭（2026-09-14）。建议补充网络层防护：NetworkPolicy 在出口再拦截元数据地址与内网段，与 netguard 形成两层（已登记为建议项）。

---

## GP-AUD-008 第三方调用无超时导致级联故障

| | |
|---|---|
| 等级 | 中 |
| CWE | CWE-400 Uncontrolled Resource Consumption |
| CVSS v3.1 | `AV:N/AC:H/PR:L/UI:N/S:U/C:N/I:N/A:H` = 5.3（中，指示性；非纯攻击者触发，由下游故障放大） |
| 位置 | [pkg/resilience/client.go](../../pkg/resilience/client.go)、[internal/order/delivery.go](../../internal/order/delivery.go) |
| 来源 | 人工审查 + `go vet` / golangci-lint（`noctx`） |

**描述**：旧版发货通知直接用 `http.DefaultClient`（无超时）同步发起请求，且对所有游戏共用一个逻辑。单个游戏服挂起会一直占用 goroutine 与连接，连接数累积后拖垮整个发货链路；对非幂等请求盲目重试还会造成重复发货。

**影响**：一个下游故障放大为发货服务整体不可用，并可能重复发货。攻击面上，能配置发货地址的运营可指向一个慢速服务器触发（PR:L 保守取值）。

**复现（针对漏洞版本）**：将某游戏的 `delivery_url` 指向一个只接受连接但不响应的服务器，持续触发发货，观察 order 服务 goroutine 与连接数持续上涨直至耗尽。

**问题代码**：

```go
func deliver(url string, body []byte) error {
	resp, err := http.DefaultClient.Post(url, "application/json", bytes.NewReader(body)) // 无超时
	if err != nil {
		return deliver(url, body) // 失败即重试，非幂等也重试
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body) // 无大小上限
	_ = data
	return nil
}
```

**修复方案**：统一经 `resilience.Client`：单次尝试超时 + 整体受 ctx 约束；仅对声明为幂等并带幂等键的请求重试，4xx 不重试；按目标 host 隔离熔断器；响应体 `io.LimitReader` 限制大小；底层使用 netguard 受保护客户端（禁代理、限重定向）。带 nonce 的签名每次尝试用 `Prepare` 重新生成。发货走独立消费者，与订单主链路解耦。

**修复后代码**：

```go
// client.go once（节选）
func (c *Client) once(ctx context.Context, r Request) (*Response, error) {
	tctx, cancel := context.WithTimeout(ctx, c.cfg.PerTryTimeout)
	defer cancel()
	req, _ := http.NewRequestWithContext(tctx, r.Method, r.URL, bytes.NewReader(r.Body))
	if r.Prepare != nil {
		r.Prepare(req) // 每次尝试重新签名
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, c.cfg.MaxBodyBytes))
	// 4xx 不重试、不计入熔断；5xx/429 返回可重试错误
}
// 熔断按 host 隔离：c.breaker(u.Host)；仅 Idempotent 请求重试
```

**回归测试**：[pkg/resilience/client_test.go](../../pkg/resilience/client_test.go) `TestBodyLimitAndTimeout`（单次超时生效、响应体截断）、`TestBreakerOpensPerHost`（熔断按 host 隔离，不影响其他 host）、`TestNoRetryForNonIdempotentOr4xx`、`TestRetryOn5xxThenSuccess`、`TestPrepareRunsPerAttempt`。

**复审结论**：已关闭（2026-09-15）。确认 gRPC 客户端也有默认超时（[pkg/grpcx/client.go](../../pkg/grpcx/client.go) `timeoutInterceptor`，仅对 UNAVAILABLE 重试），HTTP Server 侧超时见 [cmd/gateway/main.go](../../cmd/gateway/main.go)。

---

## GP-AUD-009 日志打印令牌与手机号明文（AI 初评高危，复核后下调）

| | |
|---|---|
| 等级 | 中（AI 初筛评为高，人工复核下调） |
| CWE | CWE-532 Insertion of Sensitive Information into Log File |
| CVSS v3.1 | `AV:N/AC:L/PR:H/UI:N/S:U/C:H/I:N/A:N` = 4.9（中） |
| 位置 | [pkg/logx/logx.go](../../pkg/logx/logx.go)、[pkg/mask/mask.go](../../pkg/mask/mask.go)、[internal/gateway/middleware.go](../../internal/gateway/middleware.go) |
| 来源 | AI 辅助初筛（评为“高”），人工复核确认存在但下调为“中” |

**描述**：旧版访问日志记录完整请求 URL 与请求头，登录日志打印手机号明文，部分错误日志打印 `Authorization` 头。AI 初筛将其标为高危（“令牌泄露可致账号接管”）。人工复核确认问题真实存在，但利用需要先获得日志平台访问权限（内部系统、权限受控、非网络可直接触达），且令牌有效期短（2 小时），不构成可批量的账号接管，故按分级标准下调为中。合规层面手机号属个人信息，需要治理。

**下调理由（判断过程，见 [risk-rating.md](risk-rating.md) 第 1.1、5 节）**：

- 不满足“无条件直接资金损失”或“可批量越权”的高危业务下限；
- 泄露发生在日志中，读取需要日志平台权限（PR:H），非匿名或普通用户可及；
- 非批量导出，属于随请求产生的零散泄露；
- 令牌短期有效，且后台权限以 `loadStaff` 实时校验，窃取令牌不等于获得后台权限。

**影响**：持有日志访问权限者可读取用户手机号、短期令牌，存在内部越权与合规风险。

**复现（针对漏洞版本）**：登录任意账号后在日志平台检索，可见 `phone=13812345678`、`Authorization: Bearer eyJ...` 等明文。

**问题代码**：

```go
logger.Info("http",
	"method", c.Request.Method,
	"url", c.Request.RequestURI,                 // 含订单号、手机号、token 等查询参数
	"headers", c.Request.Header)                 // 含 Authorization
logger.Info("login", "phone", req.Phone)         // 明文手机号
```

**修复方案**：统一在 slog `ReplaceAttr` 中按字段名脱敏，业务代码即使误打也不会明文落盘；访问日志记录路由模板而非原始 URL，不记请求头与请求体；慢 SQL 只打占位符（见 [dbx.go](../../pkg/dbx/dbx.go) `ParameterizedQueries`）；自由文本 / 外发 LLM 的内容用 `mask.Text` 兜底。

**修复后代码**：

```go
// logx.go
func maskAttr(_ []string, a slog.Attr) slog.Attr {
	if a.Value.Kind() == slog.KindGroup || !mask.IsSensitiveKey(a.Key) {
		return a
	}
	masked, _ := mask.ByKey(a.Key, a.Value.String())
	return slog.String(a.Key, masked)
}

// middleware.go accessLog：记录路由模板，不记原始 URL / 头
logger.InfoContext(c.Request.Context(), "http",
	"method", c.Request.Method, "route", c.FullPath(), "status", c.Writer.Status(),
	"duration_ms", time.Since(start).Milliseconds(), "client_ip", c.ClientIP(), "user_id", c.GetUint64(ctxUserID))
```

**回归测试**：[pkg/logx/logx_test.go](../../pkg/logx/logx_test.go) `TestSensitiveFieldsMasked`（phone/password/token 不明文出现，request_id/user_id 保留）；[pkg/mask/mask_test.go](../../pkg/mask/mask_test.go) `TestByKey`、`TestText`、`TestJSON`。

**复审结论**：已关闭（2026-09-16）。新增敏感字段需同步维护 `mask.sensitiveKeys`（已写入 [checklist.md](checklist.md) 第 6 节）。下调决定由安全负责人确认并记录。

---

## GP-AUD-010 钱包并发扣减导致透支

| | |
|---|---|
| 等级 | 高 |
| CWE | CWE-362 Concurrent Execution using Shared Resource with Improper Synchronization（竞态）；关联 CWE-367 |
| CVSS v3.1 | `AV:N/AC:H/PR:L/UI:N/S:U/C:N/I:H/A:N` = 5.3（中）；按业务下限（需竞态即可造成资金损失）上调为**高** |
| 位置 | [internal/wallet/service.go](../../internal/wallet/service.go) `changeOnce` |
| 来源 | 人工审查 |

**描述**：旧版扣减为“读出余额 → 在 Go 里判断够不够 → 写回新余额”。并发请求读到同一旧余额后分别写回，造成丢失更新与透支（花掉比余额更多的平台币）。

**影响**：用户并发下单 / 消费可花掉超过余额的平台币，平台直接资金损失。利用需要并发（AC:H），但可用脚本稳定复现。

**复现（针对漏洞版本）**：账户余额 100，并发发起 30 个“扣减 10”的请求，旧版会有多于 10 笔成功，账户余额变为负数。

**问题代码**：

```go
func (s *Service) Debit(ctx context.Context, req ChangeRequest) (*ChangeResult, error) {
	var acc Account
	s.db.Where("user_id = ?", req.UserID).Take(&acc)
	if acc.Available < req.Amount {
		return nil, ErrInsufficientBalance
	}
	acc.Available -= req.Amount       // 读-改-写，并发下丢失更新
	s.db.Save(&acc)                   // 覆盖式写回，无版本校验
	// ... 写流水 ...
}
```

**修复方案**：三层。(1) 幂等：先查 `(biz_type, biz_id)`，命中直接返回原结果，参数不一致拒绝；(2) 乐观锁 + 余额条件更新：`UPDATE ... WHERE id=? AND version=? AND available + ? >= 0`，`RowsAffected=0` 则有限次随机退避重试，SQL 层二次兜底防透支；(3) 流水唯一索引 `uk_biz`，并发同单号在插入时被拦下、整个事务回滚（含余额更新）。账户首建放在事务外，规避 MySQL RR 快照问题。

**修复后代码**：

```go
// changeOnce（节选）
if acc.Available+delta < 0 {
	return ErrInsufficientBalance
}
r := tx.Model(&Account{}).
	Where("id = ? AND version = ? AND available + ? >= 0", acc.ID, acc.Version, delta).
	Updates(map[string]any{
		"available": gorm.Expr("available + ?", delta),
		"version":   gorm.Expr("version + 1"),
		"updated_at": s.now(),
	})
if r.Error != nil {
	return r.Error
}
if r.RowsAffected == 0 {
	return errRetry // 快照过期，整个事务重来
}
// 写流水：并发同单号在此被 uk_biz 拦下，事务回滚
if err := tx.Create(&l).Error; err != nil {
	if dbx.IsDuplicate(err) {
		return errRetry // 重试时命中幂等分支
	}
	return err
}
```

**回归测试**：[internal/wallet/service_test.go](../../internal/wallet/service_test.go) `TestConcurrentDebitsNeverOverdraw`（余额 100，30 个并发扣 10，恰好 10 成功 20 失败，最终 0）、`TestOptimisticLockConflictRetries`（快照过期后用新快照重试）、`TestConcurrentSameBizIDCreditsOnce`（同单号并发只入账一次）、`TestCreditIsIdempotent`。

**复审结论**：已关闭（2026-09-15）。遗留说明：单元测试使用单连接 SQLite（串行），正确性逻辑已覆盖，但 MySQL 行锁 / 间隙锁的真实并发行为需 MySQL 集成测试补充（见文末遗留风险）。对账（GP 无独立编号，见 `TestReconcileDetectsMismatch`）作为兜底，可发现任何绕过服务直接改余额的不一致。

---

## GP-AUD-011 TCC 空回滚与悬挂

| | |
|---|---|
| 等级 | 中 |
| CWE | CWE-662 Improper Synchronization（分布式事务状态处理不当） |
| CVSS v3.1 | `AV:N/AC:H/PR:L/UI:N/S:U/C:N/I:L/A:L` = 4.2（中） |
| 位置 | [internal/wallet/tcc.go](../../internal/wallet/tcc.go) |
| 来源 | 人工审查 |

**描述**：TCC 冻结资金场景下，旧版 Try/Confirm/Cancel 未处理三类异常：幂等（协调者超时重试导致重复调用）、空回滚（Try 未到达协调者却发起 Cancel）、悬挂（空回滚先完成、迟到的 Try 才到达并冻结资金，这笔冻结再无 Confirm/Cancel 处理，被永久冻住）。

**影响**：资金被错误或永久冻结（可用余额凭空减少），需人工介入恢复；重复 Confirm 可能重复扣减。资金不丢失但被冻结，按分级定为中。

**复现（针对漏洞版本）**：先对某 `xid` 调用 `CancelFreeze`（Try 丢失，空回滚），再对同一 `xid` 调用 `TryFreeze`；旧版会执行冻结，该冻结此后无人处理。

**问题代码**：

```go
func (s *Service) TryFreeze(ctx context.Context, xid string, userID uint64, amount int64) (string, error) {
	// 不记录分支、不判重：迟到的 Try 也会照常冻结
	return "", s.db.Model(&Account{}).
		Where("user_id = ? AND available >= ?", userID, amount).
		Updates(map[string]any{"available": gorm.Expr("available - ?", amount), "frozen": gorm.Expr("frozen + ?", amount)}).Error
}

func (s *Service) CancelFreeze(ctx context.Context, xid string) (string, error) {
	// 查不到冻结记录时直接返回成功，不留痕 → 迟到 Try 无法识别
	return "", nil
}
```

**修复方案**：引入 `wallet_tcc_branch` 分支表，`xid` 唯一（`uk_xid`），同时承担 Try 幂等、空回滚记录与防悬挂判定。空回滚时插入 `cancelled` 状态分支；迟到的 Try 插入撞唯一键，读到 `cancelled` 即 `ErrBranchRejected`。Confirm/Cancel 用 `WHERE status='tried'` 的状态 CAS 保证并发下只有一个成功。Try 因余额不足失败时整个事务回滚，不留分支（后续 Cancel 走空回滚）。

**修复后代码**：

```go
// TryFreeze（节选）：撞唯一键后按已有分支状态处理
if err := tx.Create(&b).Error; err != nil {
	if !dbx.IsDuplicate(err) {
		return err
	}
	existing, err := s.branch(tx, xid)
	if err != nil {
		return err
	}
	if existing.Status == BranchCancelled {
		return ErrBranchRejected // 防悬挂
	}
	if existing.UserID != userID || existing.Amount != amount {
		return ErrIdempotencyConflict
	}
	status = existing.Status
	return nil // Try 幂等
}

// CancelFreeze（节选）：空回滚
b, err := s.branch(tx, xid)
if errors.Is(err, ErrBranchNotFound) {
	if err := tx.Create(&TCCBranch{XID: xid, Status: BranchCancelled}).Error; err != nil {
		if dbx.IsDuplicate(err) {
			return errRetry
		}
		return err
	}
	status = BranchCancelled
	return nil
}
```

**回归测试**：[internal/wallet/service_test.go](../../internal/wallet/service_test.go) `TestTCCEmptyRollbackAndSuspension`（空回滚成功、迟到 Try 被拒）、`TestTCCTryConfirm`（Try/Confirm 幂等、Confirm 后 Cancel 被拒）、`TestTCCTryCancel`、`TestTCCTryInsufficientRollsBackBranch`（余额不足不留分支）。

**复审结论**：已关闭（2026-09-16）。确认冻结、确认、取消均 `version + 1`，与 Credit/Debit 共用乐观锁；对账公式 `SUM(流水) = 可用 + 冻结` 仍成立（Confirm 才写流水）。

---

## GP-AUD-012 Kafka 消费失败被跳过导致消息丢失 / 重复消费

| | |
|---|---|
| 等级 | 高 |
| CWE | CWE-755 Improper Handling of Exceptional Conditions；关联 CWE-391 |
| CVSS v3.1 | `AV:N/AC:H/PR:N/UI:N/S:U/C:N/I:H/A:N` = 5.9（指示性；由 broker / 处理异常触发，非攻击者直接发起）；按业务下限（导致资金不一致）定为**高** |
| 位置 | [pkg/mq/consumer.go](../../pkg/mq/consumer.go)、[pkg/mq/kafka.go](../../pkg/mq/kafka.go)、[pkg/mq/dedup.go](../../pkg/mq/dedup.go) |
| 来源 | 人工审查 |

**描述**：旧版消费者用带消费组的 `ReadMessage`（读取即自动提交位点），处理失败就记日志后 `continue`。由于 Kafka 位点按分区单调，跳过一条后继续消费、后续位点一提交，这条失败消息就被“隐式确认”而永久丢失。叠加消费侧无幂等，重启后重放还会重复入账。

**影响**：

- 丢失 `order.paid` 事件 → 用户已付款但不到账（充值消费组）或任务进度丢失；
- 无幂等下重复投递 → 重复入账，平台资金损失。

两者都造成资金 / 账务不一致。

**复现（针对漏洞版本）**：令某条 `order.paid` 处理时数据库瞬时不可用，旧版跳过该消息并提交后续位点；数据库恢复后该用户的充值永久丢失，仅能靠人工对账发现。

**问题代码**：

```go
r := kafka.NewReader(kafka.ReaderConfig{Brokers: brokers, GroupID: "wallet-recharge", Topic: topic})
for {
	msg, err := r.ReadMessage(ctx) // 读取即自动提交位点
	if err != nil {
		return err
	}
	if err := handle(ctx, msg); err != nil {
		log.Printf("handle failed: %v", err) // 失败即跳过，消息永久丢失
		continue
	}
}
```

**修复方案**：

1. `FetchMessage`（不自动提交）+ `CommitInterval:0`（同步提交），由框架在“成功或已写入死信”之后才提交；
2. `process`：有限次重试 → 死信 → 提交；死信写失败则不提交、带退避重试写死信，阻塞该分区并告警，绝不隐式确认；
3. 永久错误（报文格式、校验失败）与 panic（recover 后）不重试，直接进死信；
4. 消费幂等：资金侧用唯一索引（`uk_biz`，biz_id = order_no），任务侧 `mq.MarkConsumed` 与业务写在同一事务；
5. ctx 取消时正在处理的消息不提交，重启重放由幂等兜底。

**修复后代码**：

```go
// consumer.go process（节选）
for attempt := 1; attempt <= c.cfg.MaxAttempts; attempt++ {
	if err = c.invoke(ctx, msg); err == nil {
		return true
	}
	if ctx.Err() != nil {
		return false
	}
	if IsPermanent(err) {
		break
	}
	// 退避后重试
}
// 死信写成功前绝不前进；写失败则阻塞分区并告警
for i := 1; ; i++ {
	if derr := c.toDLQ(ctx, msg, err); derr == nil {
		return true
	}
	c.logger.ErrorContext(ctx, "mq publish to DLQ failed, partition blocked", "attempt", i)
	if !sleep(ctx, c.cfg.Backoff(min(i, 5))) {
		return false
	}
}

// dedup.go：与业务写同一事务
func MarkConsumed(tx *gorm.DB, consumer, msgID string) (bool, error) {
	err := tx.Create(&Consumed{Consumer: consumer, MsgID: msgID}).Error
	if dbx.IsDuplicate(err) {
		return false, nil
	}
	return err == nil, err
}
```

**回归测试**：[pkg/mq/consumer_test.go](../../pkg/mq/consumer_test.go) `TestNoCommitBeforeDLQSucceeds`（死信写成功前不提交，事件顺序为 dlq→commit）、`TestExhaustedRetriesGoToDLQ`、`TestPermanentErrorAndPanicSkipRetry`、`TestCancelDuringRetryDoesNotCommit`、`TestRetryThenSucceed`；幂等侧 [internal/task/service_test.go](../../internal/task/service_test.go) `TestProgressFromEventsIsIdempotent`、[internal/wallet/service_test.go](../../internal/wallet/service_test.go) `TestHandleOrderPaid`。

**复审结论**：已关闭（2026-09-17）。确认三个消费组（wallet-recharge、task-progress、order-delivery）均配置了死信主题 `gamepub.order.paid.dlq`（`Consumer.Run` 在未配置死信时直接返回错误），生产者 `acks=all` 且按 user_id 分区保序。建议补充死信积压告警与重放工具（登记为建议项）。

---

## GP-AUD-013 支付回调按订单号查询被规则误报（不成立 / 已抑制）

| | |
|---|---|
| 等级 | 不成立（设计如此，规则抑制） |
| 关联规则 | Semgrep `order-lookup-without-owner`；AI 初筛同时报为“疑似水平越权（CWE-639）” |
| 位置 | [internal/order/callback.go](../../internal/order/callback.go) `applyPayNotify` |
| 来源 | Semgrep + AI 初筛 |

**描述**：GP-AUD-001 的归属校验规则会命中支付回调中的 `tx.Where("order_no = ?", n.OrderNo)`——它只按订单号查询，没有 `user_id` 条件。AI 初筛也把它标为疑似水平越权。

**人工复核结论：不成立**。理由：

1. 支付回调由**渠道签名**鉴权（HMAC + 时间窗 + nonce，见 GP-AUD-006），不存在用户会话，天然没有 `user_id` 可用；要求此处带 `user_id` 在语义上不成立。
2. 该查询在验签通过之后执行，攻击者无法伪造合法请求命中任意订单号（伪造需要渠道密钥）。
3. 业务侧另有渠道一致、金额一致、状态 CAS、渠道交易号唯一等校验兜底。

因此在代码中以 `nosemgrep` 抑制并写明理由，保留规则对其余调用点的覆盖：

```go
// 回调由渠道签名鉴权，不存在用户会话，按订单号查询是预期行为
// nosemgrep: order-lookup-without-owner
if err := tx.Where("order_no = ?", n.OrderNo).Take(&o).Error; err != nil {
	// ...
}
```

**处理**：标记为“不成立”，按 [risk-rating.md](risk-rating.md) 第 5 节，AI / 工具报出的问题一律人工复核后才能关闭，不自动关闭。本条作为误报登记，用于统计规则误报率；抑制点集中、带理由，便于后续审查复查。此类“按单号查询”的设计例外全仓库仅此一处。

---

## 遗留风险与建议

以下为非阻断项，已登记跟踪，设定复评时间（见 [risk-rating.md](risk-rating.md) 第 2 节）。

1. **MySQL 并发行为未被测试覆盖（高优先建议）**：单元测试使用内存 SQLite 单连接串行执行（[internal/testkit/testkit.go](../../internal/testkit/testkit.go)），能验证幂等、乐观锁、状态机的业务逻辑，但无法复现 MySQL 的行锁、间隙锁与 RR 快照（GP-AUD-010 的真实并发路径、`ensureAccount` 放在事务外的原因都依赖这些语义）。建议搭建 MySQL 集成测试环境，补充高并发扣减、并发回调、唯一键冲突回滚等用例。

2. **JWT 无主动吊销机制**：令牌有效期 2 小时，期间无法主动失效（登出、强制下线、密钥轮换前的旧令牌）。后台权限已靠 `loadStaff` 实时校验缓解，但玩家侧令牌仍无法即时作废。建议引入基于 jti（令牌已有 `ID` 字段）的吊销名单（Redis，TTL 到令牌过期），或缩短 TTL + 引入刷新令牌。

3. **人工调账缺少双人复核**：`POST /admin/v1/wallet/adjust` 已有独立权限点、原因、幂等键、单笔上限 ±100000 与审计日志（[handler_admin.go](../../internal/gateway/handler_admin.go)），但单个 finance 账号即可放行上限内的调账。建议对调账引入双人审批（maker-checker）或大额二次确认，降低内部作恶与账号被盗风险。

4. **`INTERNAL_TOKEN` 全局共享**：所有服务共用一个内部令牌，只能区分“集群内 / 外部”，不能限制调用方只能调用被授权的服务和方法。建议在服务网格层用 AuthorizationPolicy 基于 mTLS 身份做方法级授权，或改为按服务签发凭据。

5. **缓存失效时效**：资料缓存 TTL 10 分钟，直接改库（绕过 `GrantRole`）时降权 / 停用最长约 12 分钟才生效。当前没有降权 / 停用命令。建议补充运维命令并在其中主动失效缓存，或为后台鉴权单独设更短 TTL。

6. **出站 SSRF 的网络层兜底（已落地）**：netguard 在应用层拦截之外，[networkpolicy.yaml](../../deploy/k8s/networkpolicy.yaml) 的 `order-egress-internet` 只放行 order 服务访问公网 80/443，并排除私网段、CGNAT 与 169.254.0.0/16 元数据地址，形成两层防护。

7. **死信处理闭环**：三个消费组都会把失败消息转入 `gamepub.order.paid.dlq`，但仓库中没有死信积压告警与重放工具。建议补充监控（死信消息数、消费延迟）与可审计的重放流程。

8. **工具与 CI 基线（已落地）**：本次多条问题（排序注入、归属校验缺失、SSRF、硬编码凭据、context.Background、defer in loop、锁误释放、Kafka 自动提交）已沉淀为 [Semgrep 自定义规则](../../security/semgrep/rules/)（16 个用例全部通过 `semgrep --test`），连同 [.golangci.yml](../../.golangci.yml)、gosec、govulncheck、Trivy 一起接入 [.gitlab-ci.yml](../../.gitlab-ci.yml) 阻断合并。复审期间 govulncheck 还发现 `golang.org/x/net` v0.57.0 与 `quic-go` v0.59.0 中存在可达漏洞，已升级到修复版本（v0.60.0 / v0.59.1）后复扫为 0。
