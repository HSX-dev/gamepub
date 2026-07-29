# 代码审查清单

用于日常 MR 审查和专项审计。每条包括“看什么 / 怎么判”，有参考实现或回归测试的附上链接。

标记说明：

- 〔自动〕CI 工具能稳定检出，审查时确认工具结果即可。
- 〔半自动〕工具或 AI 初筛给出候选，需要人工判定是否成立。
- 〔人工〕目前只能靠人看，是审查的重点。

工具基线：`go vet`、`golangci-lint`（配置见 [.golangci.yml](../../.golangci.yml)，含 gosec、bodyclose、rowserrcheck、sqlclosecheck、noctx、errcheck、errorlint、contextcheck）、`govulncheck`、Trivy（[security/trivy.yaml](../../security/trivy.yaml)），以及 [security/semgrep/rules/](../../security/semgrep/rules/) 下的 Semgrep 自定义规则（下文统称“Semgrep 自定义规则”，每条规则在 `security/semgrep/testdata/` 有 `ruleid`/`ok` 用例，CI 先跑 `semgrep --test` 再扫描）：

| 规则 ID | 文件 | 对应小节 |
|---|---|---|
| `gorm-sql-string-concat`、`gorm-identifier-from-request` | sqli.yaml | 2. 注入 |
| `order-lookup-without-owner`、`user-id-from-request`、`jwt-parse-without-valid-methods` | authz.yaml | 1. 鉴权与越权 |
| `ssrf-unvalidated-outbound-url`、`http-client-without-timeout`、`tls-insecure-skip-verify` | ssrf.yaml | 3. SSRF 与出站请求 |
| `log-sensitive-value`、`hardcoded-credential`、`password-fast-hash` | sensitive-data.yaml | 6. 敏感信息 |
| `context-background-in-request-path`、`defer-in-loop`、`redis-lock-unsafe-release`、`redis-setnx-then-expire`、`kafka-readmessage-autocommit` | concurrency.yaml | 8–10 并发 / 锁 / 消息队列 |

规则抑制必须写成 `nosemgrep: <规则 ID>` 并在上一行注释理由，例如 [callback.go](../../internal/order/callback.go) 中的 `nosemgrep: order-lookup-without-owner`。

MR 审查不要求逐条过完整清单：先看改动涉及哪些板块，再看对应小节。专项审计按全部小节执行。

---

## 1. 鉴权与越权

- [ ] **新增路由是否落在受保护分组里** 〔自动〕：路由必须挂在 `user` 或 `admin` 分组上；需要公开的接口必须加入 `publicRoutes` 白名单，并在 MR 中说明理由。`TestEveryNonPublicRouteRequiresAuth` 会遍历全部路由，漏挂时 CI 失败。参考：[router.go](../../internal/gateway/router.go)，[router_test.go](../../internal/gateway/router_test.go)。
- [ ] **后台路由是否挂了 `requirePerm`，权限点是否正确** 〔半自动〕：测试只能断言“玩家和停用员工返回 403”，权限点选错（例如调账接口挂了 `order:read`）测不出来，需要人工对照 [rbac.go](../../internal/rbac/rbac.go) 核对。高风险操作（调账、改发货地址）应使用独立权限点。
- [ ] **`user_id` 的来源** 〔半自动〕：玩家接口只能使用 `userID(c)`（来自令牌）。在 handler 中搜索 `c.Query("user_id")`、`c.Param("user_id")` 以及请求结构体里的 `UserID` 字段，出现即需说明。参考：`TestUserIDAlwaysComesFromToken`。
- [ ] **按 ID 查询资源时带归属条件** 〔半自动，Semgrep 自定义规则〕：`Where("order_no = ?", no)` 必须改为 `Where("order_no = ? AND user_id = ?", no, uid)`。他人资源与不存在的资源返回同一个 NotFound，不能返回 403（否则会泄露资源存在性）。确属设计如此的（如支付回调按订单号查询）要加 `nosemgrep` 并写明理由。参考：[order/service.go](../../internal/order/service.go) `GetOrder` / `BatchGetOrders`，测试 `TestHorizontalPrivilegeEscalationBlocked`。
- [ ] **更新、删除也带归属条件** 〔人工〕：`UPDATE ... WHERE id = ?` 同样是越权写入点，并且要检查 `RowsAffected`。参考：[task/service.go](../../internal/task/service.go) `ClaimReward` 的条件更新。
- [ ] **批量赋值** 〔人工〕：绑定请求的结构体只声明允许修改的字段，不能直接把请求绑定到 GORM 模型再 `Save` / `Updates(struct)`。参考：[handler_user.go](../../internal/gateway/handler_user.go) `updateMeReq`，测试 `TestMassAssignmentIgnored`。
- [ ] **引用其他对象时校验归属** 〔人工〕：如头像 key 必须位于 `avatars/{uid}/` 下且不含 `..`。参考：[account/service.go](../../internal/account/service.go) `UpdateProfile`，测试 `TestAvatarMustBelongToUser`。
- [ ] **令牌中的角色不作为授权依据** 〔人工〕：后台权限以 account 的实时数据为准（`loadStaff`）。新代码不能读取 `claims.Roles` 做判断。参考：`TestAdminRoutesRequireStaffAndPermission` 最后一段。
- [ ] **JWT 校验参数** 〔半自动〕：`WithValidMethods` 限定算法，issuer、audience、exp 必须校验；所有失败折叠为同一个错误。参考：[jwt.go](../../pkg/auth/jwt.go)，测试 `TestRejectsAlgNone`、`TestRejectsHS256SignedWithPublicKey`。
- [ ] **登录与账号枚举** 〔人工〕：用户不存在、口令错误、账号停用返回同一错误，且耗时一致（对不存在的用户也做一次 bcrypt 比较）；口令超过 72 字节显式拒绝（bcrypt 会截断）。参考：[account/service.go](../../internal/account/service.go) `Login`、`validatePassword`。
- [ ] **限流 key 的维度** 〔人工〕：登录要同时按 IP 和账号限流；按 IP 限流的前提是 `TrustedProxies` 配置正确，否则可以伪造 `X-Forwarded-For` 绕过。测试：`TestLoginRateLimitedPerPhone`、`TestSpoofedForwardedForIgnored`。
- [ ] **批量与列表接口有上限** 〔人工〕：数组长度上限（binding `max=`，服务层再校验一次）、分页大小上限、深分页上限。参考：`MaxBatch`、[sqlsafe.Page](../../pkg/sqlsafe/sqlsafe.go)，测试 `TestBatchQueryLimit`、`TestPage`。

## 2. 注入（SQL、命令、日志）

- [ ] **SQL 拼接** 〔自动，gosec G201/G202 + Semgrep 自定义规则〕：`Raw` / `Exec` / `Where` 的第一个参数中出现 `fmt.Sprintf` 或 `+` 拼接外部输入即判定为问题。值一律使用 `?` 占位。
- [ ] **标识符位置的输入（排序字段、列名、表名）** 〔半自动〕：参数化保护不了标识符。`db.Order(c.Query("sort"))`、`Order(field + " " + dir)` 是 GORM 中最常见的注入点。必须经白名单映射为代码常量，并用 `clause.OrderByColumn` 构造。参考：[sqlsafe.go](../../pkg/sqlsafe/sqlsafe.go) `SortFields.OrderBy` / `Resolve`，测试 `TestOrderByWhitelist`、`TestAdminListOrdersFilters`、`TestListLedgerSortWhitelist`。
- [ ] **GORM 中容易被误认为安全的用法** 〔人工〕：`Where(map)` 的 key、`Select(userInput)`、`Group` / `Having`、`clause.Expr{SQL: ...}`、`gorm.Expr(拼接串)` 都会把字符串当作 SQL 片段处理。
- [ ] **LIKE 通配符** 〔人工〕：用户输入要先 `sqlsafe.EscapeLike` 再拼 `%`，否则 `%` 可以触发全表扫描或扩大匹配范围。
- [ ] **枚举类过滤参数走白名单** 〔人工〕：status、biz_type、game_id 先按白名单或正则校验，非法值直接返回参数错误。参考：`validStatus`、`gameIDPattern`、`ledgerBizTypes`。
- [ ] **命令执行** 〔自动，gosec G204〕：本项目不应出现 `os/exec`；确有需要时参数以数组形式传入，不经过 shell，可执行文件路径固定。
- [ ] **日志注入** 〔半自动〕：外部传入的值写日志前要做格式校验或作为结构化字段输出，不能拼进 message。参考：[middleware.go](../../internal/gateway/middleware.go) `requestID` 的格式校验，测试 `TestRequestIDSanitized`。
- [ ] **存储型 XSS 的输入端** 〔人工〕：昵称、游戏名等会在运营后台或游戏内展示的字段，拒绝控制字符与 HTML 元字符。参考：`validateNickname`、order 包的 `validName`。

## 3. SSRF 与出站请求

- [ ] **服务端发起的请求，URL 是否可被外部影响** 〔半自动，gosec G107〕：Webhook 地址、回调地址、图片 URL 抓取等都要经过 [netguard](../../pkg/netguard/netguard.go)。
- [ ] **保存配置时和建立连接时都要校验** 〔人工〕：只在保存时调用 `ValidateURL` 不够，DNS rebinding 和 302 跳转都能绕过；实际请求必须使用 `Guard.HTTPClient`，在 `Dialer.Control` 中校验最终 IP。投递前再校验一次库中地址（规则可能已收紧）。参考：[delivery.go](../../internal/order/delivery.go) `HandleOrderPaidDelivery`，测试 `TestRedirectToInternalBlocked`、`TestHTTPClientBlocksLoopbackAtDialTime`。
- [ ] **地址判定覆盖完整** 〔人工〕：回环、私网、链路本地（含云元数据地址）、CGNAT、IPv4-mapped IPv6、NAT64、6to4；域名解析出多个地址时，任一不合规即拒绝。测试：`TestIsPublic`、`TestValidateURL`。
- [ ] **协议、端口、凭据** 〔人工〕：只允许 http/https，端口白名单 80/443，URL 中不能带 `user:pass@`。
- [ ] **代理与重定向** 〔人工〕：受保护的 Transport 必须 `Proxy: nil`（走代理时 Dialer 校验的是代理地址）；限制重定向次数，并校验重定向的协议。
- [ ] **响应回显** 〔人工〕：探测类接口只返回状态码；不可达统一返回 0，具体错误只记日志，避免接口变成内网探测的回显。参考：[order/service.go](../../internal/order/service.go) `TestDelivery`、[delivery.go](../../internal/order/delivery.go) `Ping`。
- [ ] **超时** 〔自动，golangci-lint noctx + Semgrep 自定义规则〕：禁止使用 `http.DefaultClient` / `http.Get`；请求必须携带 ctx；`http.Client.Timeout` 与单次尝试超时都要设置。参考：[resilience/client.go](../../pkg/resilience/client.go)，测试 `TestBodyLimitAndTimeout`。
- [ ] **重试** 〔人工〕：只有声明为幂等（`Idempotent: true`）并携带幂等键的请求才重试；4xx 不重试；带 nonce 的签名必须每次尝试重新生成（`Prepare`）。测试：`TestNoRetryForNonIdempotentOr4xx`、`TestPrepareRunsPerAttempt`。
- [ ] **熔断粒度** 〔人工〕：熔断器按目标 host 隔离，一个游戏服故障不影响其他游戏。测试：`TestBreakerOpensPerHost`。
- [ ] **响应体上限** 〔人工〕：`io.ReadAll` 前要套 `io.LimitReader`。

## 4. 文件上传

- [ ] **类型判断依据** 〔人工〕：只看文件头魔数（`http.DetectContentType`），并用 `image.DecodeConfig` 的格式交叉验证；不信任文件名后缀和客户端提交的 Content-Type。参考：[upload.go](../../pkg/upload/upload.go) `Inspect`。
- [ ] **大小限制分两层** 〔人工〕：网关用 `bodyLimit` 限制请求体，业务层用 `LimitReader(MaxBytes+1)` 判断是否超限。测试：`TestAvatarUpload` 中的超大文件用例。
- [ ] **像素炸弹** 〔人工〕：解码前先用 `DecodeConfig` 检查宽高。测试：`TestInspect` 的 `pixel bomb` 用例。
- [ ] **重新编码** 〔人工〕：完整解码后重新编码，去掉 EXIF（含 GPS）和附加在图片后面的任意内容。测试：`TestInspect` 的 trailing payload 用例。
- [ ] **存储 key 由服务端生成** 〔半自动，gosec G304〕：不使用用户提交的文件名；`ValidKey` 校验格式；本地存储再用 `filepath.Rel` 确认不越出根目录。参考：`NewKey`、`LocalStorage.Put`，测试 `TestLocalStorageRejectsTraversal`。
- [ ] **存储与访问** 〔人工〕：私有桶 + 签名 URL，或者使用独立域名并设置 `Content-Disposition` / `nosniff`；存储目录不能位于 Web 服务器可解析执行的路径下；文件权限 0600，目录 0750（gosec G301/G306）。

## 5. 回调与签名

- [ ] **对原始报文验签** 〔人工〕：不能先反序列化再序列化后验签；网关要原样透传 body 与签名头。参考：[handler_trade.go](../../internal/gateway/handler_trade.go) `payNotify`。
- [ ] **验签不能 fail-open** 〔人工〕：签名头缺失必须拒绝；不能出现 `if sig != "" { verify }` 这种写法。
- [ ] **常量时间比较** 〔半自动，Semgrep 自定义规则〕：签名、令牌比较使用 `hmac.Equal` / `subtle.ConstantTimeCompare`，不能用 `==` 或 `bytes.Equal`。
- [ ] **防重放** 〔人工〕：时间戳窗口 + nonce 去重；nonce 在验签通过之后才占用（否则伪造请求可以提前占掉合法 nonce）；去重存储不可用时拒绝。测试：`TestVerify`、`TestForgedRequestDoesNotBurnNonce`、`TestNonceStoreFailureFailsClosed`。
- [ ] **业务失败释放 nonce** 〔人工〕：渠道重试通常原样重发报文，nonce 不释放会导致“已付款但永远不到账”。测试：`TestPayNotifyRejections` 中“同一 nonce 重试”的用例。
- [ ] **业务校验** 〔人工〕：签名合法不代表内容可信，要校验订单存在、渠道一致、金额一致；状态推进用 CAS；渠道交易号唯一。参考：[callback.go](../../internal/order/callback.go) `applyPayNotify`，测试 `TestPayNotifyRejections`、`TestPayNotifyHappyPathAndIdempotency`。
- [ ] **渠道白名单** 〔人工〕：`:channel` 路径参数必须先在已配置的验签器中命中，未知渠道直接拒绝。
- [ ] **应答语义** 〔人工〕：只有真正处理成功（含幂等命中）才应答 success；内部错误应答 fail，让渠道重试。

## 6. 敏感信息（日志、错误响应、存储、配置）

- [ ] **日志字段** 〔半自动〕：不记录请求体与原始 URL；敏感值使用约定字段名（`phone`、`token`、`password` 等），由 `logx` 自动脱敏；不要把敏感值拼进 message 或换用不在列表中的字段名。参考：[logx.go](../../pkg/logx/logx.go) `maskAttr`、[mask.go](../../pkg/mask/mask.go)，测试 `TestSensitiveFieldsMasked`、`TestByKey`。
- [ ] **新增敏感字段名** 〔人工〕：业务新增了敏感字段（如身份证、银行卡的新命名）时，同步补充 `mask.sensitiveKeys`。
- [ ] **自由文本兜底** 〔人工〕：错误信息、堆栈、外发给 LLM 的代码片段与日志，外发前使用 `mask.Text` 处理。测试：`TestText`。
- [ ] **错误响应不泄露内部细节** 〔半自动〕：SQL 错误、内网地址、文件路径和行号不能出现在响应里。gRPC 层由 `sanitizeInterceptor` 兜底，HTTP 层由 `grpcFail` 把 5xx 统一替换为通用文案。测试：`TestInternalErrorsAreSanitized`、`TestInternalErrorsNotLeaked`。
- [ ] **领域错误文案** 〔人工〕：4xx 的错误文案会透传给客户端，`toStatus` 中只能放显式构造的领域错误，不能把底层 `err.Error()` 包进 InvalidArgument 之类的状态。
- [ ] **panic** 〔人工〕：堆栈只进日志，响应只返回通用错误。参考：`recovery`、`recoveryInterceptor`。
- [ ] **存储** 〔人工〕：手机号等字段使用密文 + 盲索引 + 掩码三列；缓存中不存口令哈希与密文。测试：`TestRegisterStoresNoPlaintextPhone`、`TestProfileCacheInvalidationAndPenetration`。
- [ ] **返回字段显式列出** 〔人工〕：handler 使用 DTO，不直接把 proto 或模型序列化返回（避免角色、状态、内部 ID 等字段外泄）。参考：`userDTO`、`orderDTO`。
- [ ] **配置与密钥** 〔自动，gosec G101 + Trivy secret 扫描〕：不能有硬编码密钥或默认密钥；必填密钥使用 `config.Secret` / `SecretBytes`，缺失时启动失败；错误信息只含变量名。
- [ ] **限流 key、缓存 key 中不含明文 PII** 〔人工〕：参考登录按手机号限流时使用 SHA-256 摘要。

## 7. 资金与幂等

- [ ] **金额来源** 〔人工〕：金额只能来自服务端（商品目录、订单表），请求结构体中不应有金额字段。测试：`TestCreateOrderUsesServerSidePrice`。
- [ ] **金额类型与范围** 〔半自动〕：整数最小单位，禁止浮点；校验 `> 0` 和单笔上限（防溢出和多输几个零）；人工调账有独立上限。参考：`wallet.MaxAmount`、`maxManualAdjust`，测试 `TestValidation`。
- [ ] **每笔资金变更都带幂等键** 〔人工〕：`(biz_type, biz_id)` 必填，由唯一索引兜底；同一键参数不一致时拒绝，不能静默返回成功。测试：`TestCreditIsIdempotent`、`TestConcurrentSameBizIDCreditsOnce`。
- [ ] **余额更新方式** 〔人工〕：禁止“读出余额、在 Go 里计算、写回”。必须是 `SET available = available + ?` 的条件更新，带版本号或余额条件，并检查 `RowsAffected`。参考：[wallet/service.go](../../internal/wallet/service.go) `changeOnce`，测试 `TestConcurrentDebitsNeverOverdraw`、`TestOptimisticLockConflictRetries`。
- [ ] **余额更新和流水在同一事务** 〔人工〕：流水插入冲突时整个事务回滚，包括已执行的余额更新。
- [ ] **MySQL RR 快照** 〔人工〕：事务内“查不到 → 插入冲突 → 再查”仍会读到旧快照。账户创建这类操作应放在事务外，参考 `ensureAccount`。
- [ ] **重试有上限、有退避、可取消** 〔人工〕：乐观锁冲突重试次数有限，带随机退避，并响应 ctx 取消。参考：`Service.retry`。
- [ ] **TCC 三个异常场景** 〔人工〕：幂等、空回滚、悬挂都要处理；Confirm / Cancel 用状态 CAS。参考：[tcc.go](../../internal/wallet/tcc.go)，测试 `TestTCCEmptyRollbackAndSuspension`。
- [ ] **状态机** 〔人工〕：所有状态迁移都是 `WHERE status = 旧状态` 的 CAS；中间态（如 `claiming`）要有补偿任务。参考：[task/service.go](../../internal/task/service.go) `ClaimReward`、`CompensateClaims`，测试 `TestConcurrentClaimsCreditOnce`、`TestCompensateStuckClaims`。
- [ ] **跨服务一致性** 〔人工〕：状态变更和“要发出的事件”必须原子（outbox 同事务），不能在事务提交后直接发 MQ。测试：`TestOutboxRelay`。
- [ ] **对账覆盖** 〔人工〕：新增资金类型或新表时，确认对账公式仍然成立（`SUM(流水) = 可用 + 冻结`）。测试：`TestReconcileDetectsMismatch`。
- [ ] **人工调账** 〔人工〕：独立权限点、原因必填、幂等键、单笔上限、审计日志；超过上限走线下审批。参考：[handler_admin.go](../../internal/gateway/handler_admin.go) `adminAdjustWallet`。

## 8. 并发与资源

详细的反例、正例和排查方法见 [go-concurrency.md](go-concurrency.md)。

- [ ] **goroutine 有退出条件** 〔人工〕：后台循环 `select` 了 `ctx.Done()`；channel 发送不会永久阻塞。
- [ ] **共享 map 有锁保护** 〔自动，`go test -race`〕。
- [ ] **锁内没有 IO** 〔人工〕：持锁期间不做网络、DB、Redis 调用。
- [ ] **锁没有被拷贝** 〔自动，go vet copylocks〕：含 `sync.Mutex` 的类型使用指针接收者。
- [ ] **资源关闭** 〔自动，bodyclose / rowserrcheck / sqlclosecheck〕：`resp.Body`、`rows`、`Stmt` 都要关闭，循环中不能用 `defer` 关闭。
- [ ] **请求链路没有使用 `context.Background()`** 〔半自动，golangci-lint contextcheck〕：脱离请求生命周期必须有理由，并改用 `context.WithoutCancel` + 自带超时。
- [ ] **HTTP Server 超时** 〔自动，gosec G112/G114〕：`ReadHeaderTimeout`、`ReadTimeout`、`WriteTimeout`、`IdleTimeout`、`MaxHeaderBytes` 都已设置。参考：[cmd/gateway/main.go](../../cmd/gateway/main.go)。
- [ ] **连接池** 〔人工〕：DB 连接池上限乘以副本数不超过 MySQL `max_connections`；Redis 配置了超时。参考：[dbx.go](../../pkg/dbx/dbx.go) `DefaultPool`、[app.go](../../internal/app/app.go) `OpenRedis`。
- [ ] **整数转换** 〔自动，gosec G115〕：`int64 → int32` 之类的窄化转换确认不会溢出；确认安全时用 `// #nosec G115 -- 理由` 抑制（参考 [order/server.go](../../internal/order/server.go) `TestDelivery`）。注意 gosec 只认 `#nosec`，`//nolint:gosec` 只对 golangci-lint 生效，单独运行 gosec 时会照样报出。

## 9. 缓存与分布式锁

- [ ] **击穿、穿透、雪崩** 〔人工〕：singleflight 合并回源、空值短 TTL 缓存、TTL 加抖动。参考：[cache.go](../../internal/account/cache.go)，测试 `TestSingleflightCollapsesConcurrentMisses`。
- [ ] **singleflight 的 ctx** 〔人工〕：回源 ctx 与第一个调用方解耦（`WithoutCancel` + 自带超时），否则第一个请求被取消会让所有等待者一起失败。
- [ ] **更新顺序** 〔人工〕：先写库再删缓存，加延迟二次删除；不能“先删缓存再写库”或“更新缓存”。测试：`TestGrantRole`（授予角色后读取不到旧缓存）。
- [ ] **Redis 故障降级** 〔人工〕：缓存读失败要回源；限流在 Redis 故障时降级为单机限流而不是全部放行；nonce 存储故障时拒绝。三种场景的降级策略不同，要分别确认。参考：`RedisLimiter.Allow`，测试 `TestFallbackWhenRedisDown`。
- [ ] **缓存内容** 〔人工〕：只缓存对外视图，不缓存口令哈希、密文、令牌。
- [ ] **加锁原子性** 〔人工〕：`SET key token NX PX ttl` 一条命令完成，不能 SETNX 后再单独 EXPIRE。参考：[lock.go](../../pkg/redislock/lock.go) `TryLock`。
- [ ] **释放与续期校验 token** 〔人工〕：用 Lua 比对 token 后再 DEL / PEXPIRE，不能直接 DEL。测试：`TestExpiredHolderCannotReleaseOthersLock`。
- [ ] **长任务续期，续期失败立即停止** 〔人工〕：参考对账 `RunOnce` 的 keepalive 和 Relay `tick` 的批次间 `Refresh`。
- [ ] **锁不是正确性保障** 〔人工〕：Redis 锁没有 fencing token，GC 停顿或网络分区时可能出现两个持有者；被锁保护的操作本身也必须是幂等的，或者有数据库约束兜底。
- [ ] **defer 释放锁时的 ctx** 〔人工〕：进程退出时 ctx 已取消，释放锁应使用 `WithoutCancel` + 短超时。参考：`Reconciler.tick`。

## 10. 消息队列

- [ ] **位点提交时机** 〔人工〕：处理成功或已写入死信之后才提交；不能使用自动提交（kafka-go 带 GroupID 的 `ReadMessage` 会自动提交）。参考：[consumer.go](../../pkg/mq/consumer.go)、[kafka.go](../../pkg/mq/kafka.go)，测试 `TestNoCommitBeforeDLQSucceeds`、`TestCancelDuringRetryDoesNotCommit`。
- [ ] **失败消息不能直接跳过** 〔人工〕：Kafka 位点按分区单调，跳过一条后提交后面的位点，等于隐式确认了这条。处理流程应为：有限重试 → 死信 → 提交；死信写失败时阻塞分区并告警。测试：`TestExhaustedRetriesGoToDLQ`。
- [ ] **永久错误不重试** 〔人工〕：报文格式错误、校验失败返回 `mq.Permanent`，直接进死信；panic 被 recover 后同样按永久错误处理。测试：`TestPermanentErrorAndPanicSkipRetry`。
- [ ] **消费幂等** 〔人工〕：按业务单号做唯一约束，或者在同一事务内调用 `mq.MarkConsumed`。不能先标记再处理，也不能先处理再在另一个事务里标记。参考：[dedup.go](../../pkg/mq/dedup.go)，测试 `TestProgressFromEventsIsIdempotent`、`TestHandleOrderPaid`。
- [ ] **消费端校验消息** 〔人工〕：处理前调用 `Validate`，上游缺陷或被污染的消息不能进入资金链路。参考：[events.go](../../internal/events/events.go)。
- [ ] **单条消息处理超时** 〔人工〕：设置 `HandlerTimeout`，防止一条慢消息卡住整个分区。
- [ ] **生产端** 〔人工〕：`acks=all`；业务 key 决定分区以保证顺序；关闭自动建 topic；状态变更后的消息经 outbox 发出。
- [ ] **新增消费组** 〔人工〕：消费组名唯一，配置了死信主题（`Consumer.Run` 未配置死信时直接返回错误）。

## 11. 依赖与供应链

- [ ] **已知漏洞** 〔自动，`govulncheck ./...`〕：只报告实际可达的调用路径；结果非空即阻断合并。若某漏洞经确认不可利用，需按 [risk-rating.md](risk-rating.md) 走风险接受流程。
- [ ] **镜像与文件系统扫描** 〔自动，Trivy〕：基础镜像 CVE、镜像内残留的密钥、错误配置。
- [ ] **新增依赖** 〔人工〕：说明必要性；查看维护状态与许可证；优先使用标准库和 `golang.org/x`；不能引入 `replace` 指向未经评审的 fork。
- [ ] **校验和** 〔自动〕：`go.sum` 已提交，CI 执行 `go mod verify`；不能关闭 `GOSUMDB`（私有模块使用 `GOPRIVATE` / `GONOSUMDB` 精确匹配）。
- [ ] **生成代码** 〔人工〕：`api/gen` 只能由 buf 生成；本地插件版本需固定（[buf.gen.yaml](../../buf.gen.yaml) 当前使用 `local` 插件，需要在 CI 中固定 protoc-gen-go / protoc-gen-go-grpc 的版本）。
- [ ] **工具版本** 〔人工〕：golangci-lint、gosec、Semgrep 等扫描工具的版本固定，升级时单独提 MR，避免规则变化悄悄改变扫描结果。

## 12. 配置与部署（K8s）

- [ ] **密钥注入** 〔人工〕：通过 Secret + `secretKeyRef` 注入，不放在 ConfigMap、镜像或启动参数中；各服务只挂载自己需要的密钥（见 [architecture.md](../architecture.md) 第 6.4 节）。
- [ ] **生产开关** 〔人工〕：`AUTO_MIGRATE=false`（生产迁移走 SQL 脚本）；`TRUSTED_PROXIES` 配置为 Ingress 网段；`DEBUG_ADDR` 保持回环地址或设为 `off`。
- [ ] **调试端口** 〔人工〕：pprof 只在独立端口和独立 ServeMux 上提供，不对外暴露；不能在任何地方 `import _ "net/http/pprof"`（gosec G108）。参考：[debug.go](../../pkg/obs/debug.go)。
- [ ] **NetworkPolicy** 〔人工〕：gRPC 端口只允许 gateway 和指定服务访问；只有 order 允许访问公网出口；在网络层也拦截云元数据地址，与 netguard 形成两层防护。
- [ ] **服务网格** 〔人工〕：mTLS 设为 STRICT；用 AuthorizationPolicy 按调用方限制可访问的服务和方法（弥补 `INTERNAL_TOKEN` 全局共享的不足）。
- [ ] **Pod 安全** 〔人工〕：`runAsNonRoot`、`readOnlyRootFilesystem`（上传目录单独挂载可写卷）、`allowPrivilegeEscalation: false`、`capabilities.drop: [ALL]`、设置资源 requests / limits。
- [ ] **优雅退出** 〔人工〕：`terminationGracePeriodSeconds` 大于代码中的优雅停止时间（gRPC 和 HTTP 均为 15 秒）；就绪探针使用 gRPC 健康检查。
- [ ] **Kafka / Redis / MySQL 账号** 〔人工〕：每个服务使用独立账号，按最小权限授权（Kafka ACL 按 topic 和消费组；MySQL 只授权自己的库）。
- [ ] **日志平台** 〔人工〕：日志查询权限按需开通，保留期限符合合规要求；脱敏规则变更后抽查日志确认生效。
