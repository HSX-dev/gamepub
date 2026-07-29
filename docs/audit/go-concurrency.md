# Go 并发与资源审查要点

本文列出 Go 后端中最常见的并发与资源问题。每一项按 **反例 → 问题 → 正例 → 如何发现** 组织，正例尽量指向仓库中的实现。对应的审查清单条目见 [checklist.md](checklist.md) 第 8–10 节。

本仓库 `go.mod` 声明 `go 1.26.0`，下文涉及版本差异的地方按 1.23 及以后的语义描述。

---

## 1. goroutine 泄漏

### 1.1 结果 channel 无人接收

反例：

```go
func fetchWithTimeout(url string) ([]byte, error) {
	ch := make(chan []byte) // 无缓冲
	go func() { ch <- fetch(url) }()
	select {
	case b := <-ch:
		return b, nil
	case <-time.After(2 * time.Second):
		return nil, errTimeout // 返回后没人再接收，上面的 goroutine 永久阻塞在发送上
	}
}
```

问题：超时分支返回后，子 goroutine 在 `ch <-` 上永久阻塞，同时 `fetch` 也没有被取消，连接和内存都不会释放。每次超时泄漏一个，流量高峰时 goroutine 数会持续上涨。

正例：把 ctx 传给真正做事的函数，由它自己响应超时；确实需要 channel 时用容量为 1 的缓冲，保证发送方一定能退出。

```go
func fetchWithTimeout(ctx context.Context, url string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return fetch(ctx, url) // fetch 内部使用 http.NewRequestWithContext
}
```

仓库中的出站请求都通过 [resilience.Client.once](../../pkg/resilience/client.go) 发出：每次尝试使用 `context.WithTimeout(ctx, PerTryTimeout)`，并且 `defer cancel()`。

### 1.2 后台循环没有退出条件

反例：

```go
func (r *Relay) Start() {
	go func() {
		for {
			r.tick()
			time.Sleep(time.Second)
		}
	}()
}
```

问题：进程收到 SIGTERM 时这个 goroutine 不会停，优雅退出只能等超时后被强杀；测试中每创建一个 Relay 就泄漏一个 goroutine；`tick` 内部也拿不到可取消的 ctx。

正例：[outbox.go](../../internal/order/outbox.go) `Relay.Run`、[reconcile.go](../../internal/wallet/reconcile.go) `Reconciler.Run`、[task/service.go](../../internal/task/service.go) `RunCompensator` 都是同一个结构：

```go
func (r *Relay) Run(ctx context.Context) error {
	t := time.NewTicker(r.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			r.tick(ctx)
		}
	}
}
```

启动方不自己 `go`，而是交给 [app.Background](../../internal/app/app.go)，由 `errgroup` 统一等待退出，任务返回错误时整个进程退出。

### 1.3 range 一个永远不会关闭的 channel

反例：

```go
jobs := make(chan Job)
for i := 0; i < 8; i++ {
	go func() {
		for j := range jobs { // 生产者从不 close(jobs)
			handle(j)
		}
	}()
}
for _, j := range list {
	jobs <- j
}
return // 8 个 worker 全部泄漏
```

问题：worker 只在 channel 关闭时退出。生产者提前返回（包括中途出错返回）时如果没有 `close`，worker 全部泄漏。

正例：由唯一的生产者负责 `defer close(jobs)`；worker 同时 `select` ctx。更简单的做法是使用 `errgroup` 加 `SetLimit`（见第 9 节），不手写 worker 池。

### 如何发现

- **运行时**：通过调试端口查看 goroutine profile。各服务的 pprof 由 [obs.ServeDebug](../../pkg/obs/debug.go) 提供，使用独立端口和独立 ServeMux，默认只监听 `127.0.0.1:6060`：

  ```bash
  kubectl port-forward pod/<order-pod> 6060:6060
  curl -s 'http://127.0.0.1:6060/debug/pprof/goroutine?debug=1' | head -50
  go tool pprof -top http://127.0.0.1:6060/debug/pprof/goroutine
  ```

  间隔几分钟各取一次，同一调用栈的数量持续增长即为泄漏。`debug=2` 会输出每个 goroutine 的完整栈和阻塞时长（如 `[chan send, 37 minutes]`）。
- **测试**：引入 `go.uber.org/goleak`（当前 `go.mod` 中还没有），在包级 `TestMain` 中调用 `goleak.VerifyTestMain(m)`。后台循环类组件（Relay、Reconciler、Consumer）优先接入。
- **审查**：每个 `go` 语句都要能回答“它什么时候退出、谁在等它退出”。

---

## 2. map 并发读写

反例：

```go
type breakers struct {
	m map[string]*gobreaker.CircuitBreaker[*Response]
}

func (b *breakers) get(host string) *gobreaker.CircuitBreaker[*Response] {
	if cb, ok := b.m[host]; ok {
		return cb
	}
	cb := newBreaker(host)
	b.m[host] = cb // 多个请求 goroutine 同时写
	return cb
}
```

问题：Go 的 map 并发读写会直接触发 `fatal error: concurrent map writes`，这是 runtime 层面的致命错误，**recover 不能捕获**，整个进程退出。低并发时可能长期不出现，到流量高峰才触发。

正例：[resilience.Client.breaker](../../pkg/resilience/client.go) 用 `sync.Mutex` 保护按 host 的熔断器 map；[ratelimit.Local](../../pkg/ratelimit/ratelimit.go) 同样用互斥锁保护限流器 map，并限制 key 数量上限，防止被大量随机 key 撑爆内存。读多写少、key 集合稳定的场景可以考虑 `sync.Map`，但大多数情况下 `Mutex` + 普通 map 更容易看懂。

如何发现：`go test -race ./...`。数据竞争只有在测试真的并发执行到时才能检测到，所以关键组件要有并发测试，例如 [wallet/service_test.go](../../internal/wallet/service_test.go) 的 `TestConcurrentDebitsNeverOverdraw`、[account/service_test.go](../../internal/account/service_test.go) 的 `TestSingleflightCollapsesConcurrentMisses`。Linux / Windows 上 `-race` 需要 `CGO_ENABLED=1` 和 C 工具链；本仓库测试使用纯 Go 的 SQLite 驱动，开启 CGO 不影响。

---

## 3. 锁粒度过大、锁内做 IO

反例：

```go
func (c *Client) Do(ctx context.Context, r Request) (*Response, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	cb := c.breakers[host]
	return cb.Execute(func() (*Response, error) { return c.once(ctx, r) }) // 持锁期间发起 HTTP 请求
}
```

问题：所有出站请求被这把锁串行化，一个慢的游戏服会拖住所有游戏的发货，等于自己实现了一个全局单线程。同样的问题还常出现在“持锁期间查 DB / 调 Redis / 写日志到网络”。

正例：锁只保护内存数据结构的读写，IO 放在锁外。[resilience.Client.breaker](../../pkg/resilience/client.go) 只在查找或创建熔断器时持锁，`Do` 中的 HTTP 调用不持锁；[ratelimit.Local.Allow](../../pkg/ratelimit/ratelimit.go) 在取到 `*rate.Limiter` 后先 `Unlock`，再调用 `lim.Allow()`（`rate.Limiter` 自带锁）。

如何发现：

- 审查时看 `Lock()` 与 `Unlock()` 之间有没有 ctx 参数的调用，有就是锁内 IO。
- 运行时开启 mutex / block profile（`runtime.SetMutexProfileFraction`、`runtime.SetBlockProfileRate`，默认关闭，按需在调试时临时开启），然后查看 `/debug/pprof/mutex`、`/debug/pprof/block`。
- `go test -race` 检测不到这类问题，它只检测数据竞争。

---

## 4. 锁拷贝

反例：

```go
type Local struct {
	mu sync.Mutex
	m  map[string]*rate.Limiter
}

func (l Local) Allow(key string) bool { // 值接收者：每次调用都拷贝了一把新锁
	l.mu.Lock()
	defer l.mu.Unlock()
	// ...
}
```

问题：值接收者调用时拷贝了整个结构体，包括 `sync.Mutex`，每次调用锁的都是一个副本，互斥完全失效。map 是引用类型，副本仍然指向同一个 map，于是变成了“看起来加了锁的并发写”。同样的问题会出现在按值传参、`for _, v := range []T{...}` 拷贝元素、把含锁结构体赋值给另一个变量时。

正例：含 `sync.Mutex` / `sync.WaitGroup` / `atomic.*` 字段的类型统一使用指针接收者，构造函数返回指针，例如 `ratelimit.NewLocal`、`signature.NewMemoryNonceStore`、`resilience.New`。

如何发现：`go vet` 的 copylocks 检查（`go test` 会自动运行部分 vet 检查，CI 中应显式执行 `go vet ./...`）。

---

## 5. 循环中的 defer

反例：

```go
for _, key := range keys {
	f, err := os.Open(filepath.Join(dir, key))
	if err != nil {
		return err
	}
	defer f.Close() // 直到函数返回才执行，循环期间文件句柄一直累积
	// ...
}
```

```go
for _, uid := range uids {
	tx := db.Begin()
	defer tx.Rollback() // 同理：事务和连接都要等整个循环结束才释放
	// ...
}
```

问题：`defer` 的作用域是函数而不是循环体。批量任务中循环几千次就会耗尽文件句柄或连接池，后续请求全部阻塞在获取连接上。

正例：把每轮迭代放进独立函数，让 defer 在每轮结束时执行。仓库中 [Reconciler.RunOnce](../../internal/wallet/reconcile.go) 对每个有差异的用户调用 `recheck`，后者在自己的 `Transaction` 闭包中完成开启、提交或回滚；[task.CompensateClaims](../../internal/task/service.go) 对每条记录调用 `issue`，同样不在循环里使用 defer。

如何发现：golangci-lint 的 `gocritic`（`deferInLoop` 检查）或 `revive`（`defer` 规则）。

---

## 6. 连接、rows、Body 未关闭

反例：

```go
resp, err := http.DefaultClient.Do(req)
if err != nil {
	return err
}
if resp.StatusCode != http.StatusOK {
	return fmt.Errorf("status %d", resp.StatusCode) // Body 未关闭，连接无法复用
}
body, _ := io.ReadAll(resp.Body) // 没有大小上限
```

```go
rows, err := db.Raw("SELECT user_id, SUM(amount) FROM wallet_ledger GROUP BY user_id").Rows()
if err != nil {
	return err
}
for rows.Next() { // 没有 defer rows.Close()，也没有检查 rows.Err()
	// ...
}
```

问题：

- `resp.Body` 不关闭会泄漏连接，以及 Transport 中负责读写该连接的 goroutine，表现为 goroutine 和 fd 缓慢增长。
- `rows` 不关闭会一直占用连接池中的连接；遍历结束后不检查 `rows.Err()`，网络中断导致的部分结果会被当作完整结果处理，在对账这类场景里会直接得出错误结论。
- 手动 `Begin()` 的事务在出错路径上没有回滚，连接一直被占用。

正例：

- [resilience.Client.once](../../pkg/resilience/client.go)：`defer resp.Body.Close()`，并用 `io.LimitReader(resp.Body, MaxBodyBytes)` 限制读取大小；任何状态码路径都会关闭 Body。
- 仓库中的聚合查询使用 GORM 的 `Scan` 读入结构体（如 [reconcile.go](../../internal/wallet/reconcile.go)），由 GORM 负责关闭 rows；确实需要 `Rows()` 流式读取时，必须写 `defer rows.Close()` 并在循环后检查 `rows.Err()`。
- 事务统一使用 `db.Transaction(func(tx *gorm.DB) error { ... })`，返回错误或发生 panic 时自动回滚。
- 进程级资源在退出时关闭：gateway 在 `Shutdown` 之后关闭全部 gRPC 连接（[cmd/gateway/main.go](../../cmd/gateway/main.go)）；Kafka reader / writer 在 `app.Background` 的闭包中 `defer Close()`（[cmd/order/main.go](../../cmd/order/main.go)）。

如何发现：golangci-lint 的 `bodyclose`、`rowserrcheck`、`sqlclosecheck`；运行时观察 `/debug/pprof/goroutine` 中 `net/http.(*persistConn).readLoop` / `writeLoop` 的数量，以及数据库连接池的 `sql.DBStats`（`InUse` 持续上涨、`WaitCount` 增加）。

---

## 7. 循环中的 time.After

反例：

```go
for {
	select {
	case msg := <-msgs:
		handle(msg)
	case <-time.After(30 * time.Second):
		return errIdle
	}
}
```

问题：

- **Go 1.23 之前**：`time.After` 创建的 timer 在触发前不会被 GC 回收。高频循环中每轮创建一个 30 秒的 timer，内存会持续累积直到这些 timer 逐个到期，这是早期 Go 服务中很常见的“内存泄漏”。
- **Go 1.23 及以后**（`go.mod` 声明 `go 1.23` 以上时生效，本仓库为 1.26）：不再被引用的 Timer / Ticker 即使没有调用 `Stop` 也可以被 GC 回收；timer channel 改为同步（无缓冲），`Reset` / `Stop` 之后不会再读到过期的值。所以上面的代码**不再泄漏内存**，但仍有两个问题：
  1. 每轮循环都分配一个新 timer，在热路径上会增加分配和 GC 压力；
  2. 语义：每次收到消息都会重新开始计时，这是“空闲超时”而不是“总超时”。如果本意是“整个操作最多 30 秒”，这里的写法就是错的，并且在流量持续的情况下永远不会超时。

正例：

- 需要总超时时，在循环外使用 `context.WithTimeout`，循环内 `select ctx.Done()`。
- 需要可复用的定时器时，使用 `time.NewTimer` 加 `defer t.Stop()`，例如 [mq/consumer.go](../../pkg/mq/consumer.go) 和 [resilience/client.go](../../pkg/resilience/client.go) 中的 `sleep` 函数；周期任务使用 `time.NewTicker` 加 `defer t.Stop()`，例如 `Relay.Run`。
- 测试代码中的轮询循环（如 [consumer_test.go](../../pkg/mq/consumer_test.go) 的 `fakeReader.Fetch`）使用 `time.After` 可以接受。

如何发现：staticcheck 和 gocritic 对部分写法有检查；审查时关注 `for` + `select` + `time.After` 的组合，先确认作者想要的是哪种超时语义。如果需要兼容旧版本行为，注意 `GODEBUG=asynctimerchan=1` 会恢复旧语义。

---

## 8. singleflight 与 ctx 耦合

反例：

```go
func (c *profileCache) get(ctx context.Context, id uint64) (*Profile, error) {
	v, err, _ := c.sf.Do(key(id), func() (any, error) {
		return c.loadFromDB(ctx, id) // 用的是第一个调用者的 ctx
	})
	// ...
}
```

问题：同一个 key 的所有并发请求共享第一个调用者的执行结果。如果第一个请求的客户端断开（ctx 被取消），回源失败，**所有等待中的请求一起收到 `context.Canceled`**。热点 key 上的一个用户断线，会让同一时刻请求该 key 的全部用户报错。

正例：[account/cache.go](../../internal/account/cache.go) 的 `get`：

```go
v, err, _ := c.sf.Do(key, func() (any, error) {
	// 与发起者的 ctx 解耦：否则第一个请求被取消会让所有合并等待的请求一起失败
	lctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	p, err := load(lctx)
	// ...
})
```

`context.WithoutCancel` 保留 ctx 中的值（如 request_id，日志仍能关联到请求），去掉取消和截止时间，因此必须自己再加一个超时。

其他需要注意的点：

- `Do` 不能被单个等待者的 ctx 中断。如果需要“我自己的请求取消了就立即返回”，改用 `DoChan` 并同时 `select ctx.Done()`。仓库中使用 `Do` 加 3 秒上限，最坏情况下等待者多等 3 秒。
- 返回值被所有等待者共享。`*Profile` 是同一个指针，调用方只能读取，不能修改；如果需要修改要先拷贝。
- 回源函数中的 panic 会传递给所有等待者。

如何发现：审查所有 `singleflight.Group.Do` 调用，检查闭包中使用的 ctx 来自哪里。测试参考 `TestSingleflightCollapsesConcurrentMisses`。

---

## 9. errgroup 误用

反例：

```go
g, gctx := errgroup.WithContext(ctx)
for _, uid := range uids { // 数量不受控
	g.Go(func() error {
		return s.syncUser(ctx, uid) // 用了外层 ctx，而不是 gctx
	})
}
_ = g.Wait() // 错误被丢弃
```

问题：

- 使用外层 `ctx` 而不是 `gctx`：一个 goroutine 出错后，其他 goroutine 不会被取消，`WithContext` 的意义就没有了。
- 没有 `SetLimit`：请求中带 1 万个 uid 就会启动 1 万个 goroutine，同时打满 DB 连接池。
- 丢弃 `Wait()` 的错误。
- 零值 `errgroup.Group{}` 不会在出错时取消其他任务，只会记录第一个错误。
- `g.Go` 中如果有阻塞调用不响应 ctx（例如 `http.Server.ListenAndServe`），`Wait()` 会永远等下去。

正例：

- [app.Main](../../internal/app/app.go) 使用 `errgroup.WithContext`，并把 `gctx` 传给所有子任务；任一任务返回错误时，其余任务通过 `gctx` 收到取消信号。
- [cmd/gateway/main.go](../../cmd/gateway/main.go) 用两个 goroutine 处理阻塞的 `ListenAndServe`：一个运行服务，另一个等待 `ctx.Done()` 后调用 `Shutdown`，这样 `ListenAndServe` 能返回，`Wait()` 能结束。[grpcx.GracefulStop](../../pkg/grpcx/server.go) 对 gRPC 做了同样的处理，并在超时后强制 `Stop`。
- 请求路径中的并发扇出必须调用 `g.SetLimit(n)`，并在入口限制输入规模（参考批量查询上限 `MaxBatch = 50`）。

如何发现：审查 `errgroup.WithContext` 后的闭包是否使用了 `gctx`；`errcheck` 可以发现被丢弃的 `Wait()` 错误；golangci-lint 的 `contextcheck` 可以发现部分 ctx 没有沿调用链传递的情况。

---

## 10. 请求链路中使用 context.Background()

反例：

```go
func (h *handlers) balance(c *gin.Context) {
	resp, err := h.Wallet.GetBalance(context.Background(), &walletv1.GetBalanceRequest{UserId: userID(c)})
	// ...
}

func (s *Service) UpdateProfile(ctx context.Context, id uint64, nickname string) error {
	// ...
	go s.audit(ctx, id) // 请求返回后 ctx 被取消，异步审计随之失败
	return nil
}
```

问题：

- 使用 `Background()`：客户端断开或上游超时后，下游调用仍在继续，资源被白白占用；request_id 等链路信息丢失，日志无法关联。gRPC 客户端的 [timeoutInterceptor](../../pkg/grpcx/client.go) 会为没有截止时间的调用补上默认 3 秒超时，但数据库、Redis 调用没有这层保护。
- 反过来，把请求 ctx 直接传给请求结束后还要继续执行的异步任务，任务会在请求返回时被取消。

正例：

- 请求链路中一律向下传递 `c.Request.Context()` / 入参 ctx。
- 确实需要脱离请求生命周期的操作，使用 `context.WithoutCancel(ctx)`（保留值，去掉取消），并**自己加超时**：
  - [account/cache.go](../../internal/account/cache.go) `invalidate`：延迟二次删除用 `time.AfterFunc` 执行，ctx 为 `WithoutCancel` 加 1 秒超时；
  - [reconcile.go](../../internal/wallet/reconcile.go) `tick` 和 [outbox.go](../../internal/order/outbox.go) `tick`：在 defer 中释放分布式锁时，原 ctx 可能已因进程退出而取消，使用 `WithoutCancel` 加 2 秒超时；
  - [obs/debug.go](../../pkg/obs/debug.go) 和 [cmd/gateway/main.go](../../cmd/gateway/main.go)：`Shutdown` 使用 `WithoutCancel` 加超时。
- `context.Background()` 只应出现在 `main`、测试和运维命令的入口（如 `cmd/account` 的 `runCommand`）。

如何发现：golangci-lint 的 `contextcheck`；Semgrep 自定义规则匹配 `internal/` 下非测试文件中的 `context.Background()` / `context.TODO()`。

---

## 11. Kafka 位点提交顺序

反例：

```go
r := kafka.NewReader(kafka.ReaderConfig{Brokers: brokers, GroupID: "wallet-recharge", Topic: topic})
for {
	msg, err := r.ReadMessage(ctx) // 设置了 GroupID 时，ReadMessage 会自动提交位点
	if err != nil {
		return err
	}
	if err := handle(ctx, msg); err != nil {
		log.Printf("handle failed: %v", err)
		continue // 位点已提交，这条消息就此丢失
	}
}
```

问题：

- `ReadMessage` 在返回消息时就已经提交了位点，处理失败后无法重新消费。
- 即使改成手动提交，“失败了跳过，继续处理下一条”也会丢消息：Kafka 位点按分区单调递增，提交 offset N+1 就等于确认了 N 及之前的所有消息。
- 如果在同一分区内并发处理，再提交“当前最大位点”，前面仍在处理中或已失败的消息同样会被隐式确认。

正例：[pkg/mq](../../pkg/mq/consumer.go)：

1. [KafkaReader](../../pkg/mq/kafka.go) 使用 `FetchMessage`（不自动提交），并设置 `CommitInterval: 0`，`CommitMessages` 返回即表示已同步提交到 broker；
2. `Consumer.process` 按“有限次重试 → 写死信 → 返回 true”的顺序处理，只有返回 true 才提交；
3. 死信写入失败时**不提交**，带退避地重试写死信，此时该分区被阻塞并输出 ERROR 日志，等待人工介入；
4. 每次处理都有 `HandlerTimeout`，panic 会被 recover 并按永久错误转入死信；
5. ctx 取消（进程退出）时，正在重试的消息不提交，重启后会被重新投递。业务侧依靠幂等处理重复投递（钱包 `uk_biz`、task 的 `MarkConsumed`）。

如果将来需要在分区内并发处理，必须按分区维护“已连续完成的最大位点”，只提交这个位点，不能直接提交最新位点。

如何发现：审查所有 `kafka.NewReader` 的配置和读取方式（`ReadMessage` 还是 `FetchMessage`）；测试参考 `TestNoCommitBeforeDLQSucceeds`、`TestCancelDuringRetryDoesNotCommit`、`TestExhaustedRetriesGoToDLQ`。

---

## 12. Redis 分布式锁误释放

反例：

```go
ok, _ := rdb.SetNX(ctx, "wallet:reconcile", 1, 0).Result() // 不带过期时间
if !ok {
	return
}
rdb.Expire(ctx, "wallet:reconcile", time.Minute) // 进程在两条命令之间崩溃 → 永久死锁
defer rdb.Del(ctx, "wallet:reconcile")           // 不校验持有者
runReconcile(ctx)                                 // 运行时间可能超过 TTL
```

问题：

- SETNX 和 EXPIRE 分两步执行，中间崩溃时锁永远不会过期。
- 任务执行时间超过 TTL 后锁自动过期并被另一个副本获取，此时这里的 `DEL` 会删掉**别人的锁**，第三个副本随后也能获取，互斥彻底失效。
- 没有续期，长任务一定会超过 TTL。
- defer 中使用请求或任务的 ctx 释放锁：进程退出时 ctx 已取消，`DEL` 直接失败，锁只能等 TTL 过期。

正例：[pkg/redislock](../../pkg/redislock/lock.go)：

- `TryLock` 使用 `SET key token NX PX ttl` 一条命令完成，token 为 16 字节随机数；
- `Unlock` / `Refresh` 通过 Lua 脚本先比对 token 再执行 `DEL` / `PEXPIRE`，token 不匹配时返回 `ErrLockLost`；
- 使用方在每批处理之后续期，续期失败立即停止：对账见 [reconcile.go](../../internal/wallet/reconcile.go) `RunOnce` 的 keepalive，发件箱见 [outbox.go](../../internal/order/outbox.go) `tick`；
- 释放锁使用 `context.WithoutCancel` 加短超时。

仍需记住：Redis 单实例锁没有 fencing token，GC 停顿或网络分区时仍可能短暂出现两个持有者。因此锁只用于减少并发，被保护的操作本身必须能容忍重复执行：对账只读取和写入差异记录，发件箱重复投递由消费端幂等处理。资金正确性不依赖这把锁。

如何发现：搜索 `SetNX` / `SETNX` / `Del(` 在锁语义下的用法；测试参考 `TestExpiredHolderCannotReleaseOthersLock`、`TestRefreshExtendsTTL`。

---

## 13. 工具汇总

| 工具 | 能发现 | 用法与限制 |
|---|---|---|
| `go test -race ./...` | 数据竞争（map 并发写、未加锁的共享变量） | 只能发现测试实际执行到的并发路径；Linux / Windows 需开启 CGO；CI 必跑 |
| `go vet ./...` | 锁拷贝（copylocks）、未调用的 cancel（lostcancel）、错误使用 resp 后才判断 err（httpresponse） | CI 必跑 |
| golangci-lint | `bodyclose`、`rowserrcheck`、`sqlclosecheck`、`noctx`、`contextcheck`、`errcheck`、`gocritic`（deferInLoop）、`staticcheck`、`nilerr` | 配置见 [.golangci.yml](../../.golangci.yml)，CI 固定镜像版本；例如 `mq.Consumer.Run` 的关闭路径就是按 `nilerr` 提示重构为 `fetch` 返回 `(msg, ok)` 的 |
| Semgrep 自定义规则 | `context-background-in-request-path`、`defer-in-loop`、`redis-lock-unsafe-release`、`redis-setnx-then-expire`、`kafka-readmessage-autocommit` | [concurrency.yaml](../../security/semgrep/rules/concurrency.yaml)，用例见 `security/semgrep/testdata/concurrency.go` |
| pprof（`obs.ServeDebug`） | goroutine 泄漏（goroutine profile）、锁竞争（mutex / block profile，需临时开启采样）、内存增长（heap） | 通过 `kubectl port-forward` 访问，端口不对外；`/debug/pprof/profile` 会消耗 CPU，线上谨慎使用 |
| goleak | 测试结束后残留的 goroutine | 需新增依赖 `go.uber.org/goleak`；优先用于后台循环类组件 |
| MySQL 集成测试 | 行锁、间隙锁、RR 快照下的并发行为 | 当前单元测试使用单连接 SQLite，无法覆盖，需要用真实 MySQL 补充 |
