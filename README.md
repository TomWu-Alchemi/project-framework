# project-framework

要求 **Go 1.26.8** 或更高（见 `go.mod` 的 `go` 行）。下游在默认 `GOTOOLCHAIN=auto` 下会按该版本选取工具链。

代码审查与修复方案：[docs/code-review.md](docs/code-review.md)（含问题清单、分批修复方案与决策记录）。

建议 Init 顺序：先 `logger.InitLogger()`，再 `cacheproxy.Init` / NATS。`CacheContext` 的 TTL 与 refresh offset **小于等于 0 表示使用默认值**（过期 24h、空值 1m、逻辑刷新 10m），不是「永不过期」或「KEEPTTL / 立即回源」；该约定对直接调用 `RedisCache.Set` 同样生效，`httpclient.NewDalHttpClient` 的 `Timeout` 小于等于 0 时回退为 10s，其 `DalLog` 为 nil 表示**不记录任何 DAL 日志**（DAL 日志为全量请求/响应记录，属显式 opt-in：未注入时不产生日志、不占用全局日志通道）。访问日志仍打 header/body/query，过大截断为 256KiB。`logger.InitLoggerWithConfig` 可配置 JSON 编码、目录、info/error/access/panic 是否同时写 stdout、lumberjack Compress、`DisableCaller`（关掉文件:行号以省掉每条日志的栈回溯），以及 `BufferSize` / `BufferFlushInterval`（文件侧内存缓冲：零值默认关闭、即同步写；只缓冲 lumberjack 文件侧，stdout 始终同步；`BufferFlushInterval` 零值回退 100ms（本库语义）；打开后进程崩溃可能丢失缓冲区）。`InitLogger()` 行为不变。热路径可用 `logger.GetLogger()` 打 `zap.Field`。`cacheproxy.WithLogger` 可为代理注入 `*zap.Logger`（nil 表示继续使用全局 logger）。已取消的 `GetHit` 调用方立即返回，不再等待共享回源结束；回源仍会跑完以服务同 key 的其他等待者。

缓存补充约定：`cacheproxy.Init(rdb, cacheproxy.WithFetchTimeout(d))` 可配置回源超时（默认 5s，`<= 0` 回退默认值）；超时的保证是「回源 ctx 携带 deadline 并传递到 getter」（无法强制中断不尊重 ctx 的 getter），超时后同一 key 的 singleflight 等待者会一并收到 `context.DeadlineExceeded`。Redis 中无法解析的缓存值按未命中处理并回源重写（自愈），同时输出 Warn 日志（含 key 与原始数据字节数，不含原始内容；每次降级均记录，不做限流）；同一 key 的过期刷新同时最多一个 goroutine；getter 的 panic 会在回源层收敛为 error 返回，不会击穿调用方。

key 命名前提：singleflight 以**缓存 key 本身**为回源合并键——不同业务模块若共用同一 key 字符串、注入不同 getter，会互相加入对方的在途回源并拿到对方的数据，且互相覆盖缓存。key 必须按数据源全局唯一（建议命名空间化，如 `biz1:user:123`）。

时钟前提：逻辑过期（`IsExpire`）基于写入方时间戳（`Ctime` 序列化进 Redis）与读取方本地时钟比较，多机部署需保证 NTP 时钟同步；机器间时钟偏差会直接影响刷新判定与旧值返回时长。
