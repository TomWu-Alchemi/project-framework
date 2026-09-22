# Changelog

本文件记录本项目的对外可见变更。格式参考 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/)。

**版本策略**：本项目处于 0.x 阶段，允许发布不兼容变更；所有不兼容变更均在对应版本的「不兼容变更」小节显式标注，升级前请务必阅读。

**历史版本索引**（本文件自 v0.0.4 起建立，历史版本内容以 git tag 为准，未在本文重复记录）：

- `v0.0.1` — 项目初始化（commit `3a26d07`）
- `v0.0.2` — 工具类 / 响应类（commit `94d738f`）
- `v0.0.3` — 等同 `master`（commit `ac54a03`）

---

## [v0.0.5] - 2026-09-22

> 基线 `v0.0.4`。升级前请先阅读「不兼容变更」。

### 不兼容变更（Breaking）

- **`cacheproxy.Init`**：签名变为 `func Init(rdb *redis.Client, opts ...Option) error`。`rdb == nil` 返回 `ErrNilRedis` 且不消耗 once，随后仍可再次 Init；重复的合法 Init 返回 `ErrAlreadyInitialized`。调用方必须处理返回值。

### 行为变更

- **`httpclient` 失败错误文本**：`failedRequest` 不再把 response body 拼进 `error.Error()`，仅保留 `status=`。日志字段里的 response 不变。
- **`httpclient` 连接上限**：默认每主机最多 256 条在途连接（`Transport.MaxConnsPerHost`）；`DalHttpClientConf.MaxConnsPerHost > 0` 时可覆盖。
- **`GetWithRetry` 重试**：仅对 429 / 500 / 502 / 503 / 504 重试（501 等其它 5xx 不再重试）。429 / 503 会参考 `Retry-After`，与现有 backoff 取较大者，最多等待 30 秒。
- **重定向**：仍然跟随，最多 10 次。跨主机跳转时额外去掉 `X-Api-Key`。`Authorization` 与 `Cookie` 仍由标准库在跨主机时剥离。
- **非 2xx 响应体**：读取上限为 256KiB，超出部分不进入日志；成功响应仍最多读取 10MB。
- **已取消的缓存回源**：进入 `GetHit` 时 `ctx` 已取消则不再启动回源。结果已经就绪时返回结果，而不是 `context.Canceled`。
- **访问日志**：同一请求的多条 gin 错误合并为一条，`msg` 仍是第一条错误文本，全部错误在 `errors` 字段。
- **日志轮转**：单次轮转 panic 后，按小时轮转的循环继续运行。
- **NATS**：连接异步错误（如 slow consumer）写入日志。

---

## [v0.0.4] - 2026-09-20

> 基线 `v0.0.3`。升级前请先阅读「不兼容变更」与「迁移清单」。

### 不兼容变更（Breaking）

- **A1 Go 版本**：`go 1.25.7` → `go 1.26.8`（见 `go.mod`）。下游需同步升级工具链。
- **A2 `httpclient.DalHttpClient.GetWithRetry` 签名变更**：
  - 旧：`GetWithRetry(baseUrl, params, headers, maxRetries)`
  - 新：`GetWithRetry(ctx context.Context, baseUrl, params, headers, maxAttempts)`
  - 新增 `ctx` 作为首个参数，末位参数语义由 `maxRetries` 改为 `maxAttempts`。
- **A3 `util.BcryptHash` 签名变更**：`func BcryptHash(pw string) string` → `func BcryptHash(pw string) (string, error)`。错误不再被吞掉，调用方必须处理返回值。
- **A4 lumberjack 依赖路径变更**：`github.com/natefinch/lumberjack v2.0.0+incompatible` → `gopkg.in/natefinch/lumberjack.v2 v2.2.1`。下游若直接 import 旧路径需同步改到新路径，避免新旧两个包同时存在于依赖图中。
- **A5 httpclient 错误体系重构**：移除 `github.com/pkg/errors`，改用标准库错误包装（`%w`）。
  - 最终错误文本由 `after N retries, last error: X` 改为 `after N attempts: X`。
  - `lastErr` 现以 `%w` 包装，可通过 `errors.Is` 命中。
  - **下游使用 `errors.Cause()` 提取根因的代码会失效，须改用 `errors.Is` / `errors.As`。**
- **A6 `logger.Shutdown()` 成为必须配对的生命周期调用**：应在「进程退出前最后一步、且在所有日志写入之后」调用——`BufferedWriteSyncer.Stop` 之后 flushLoop 已停止，此后再写日志只会进入内存、永不落盘。该调用幂等，可重复执行。
  - 使用 `BufferSize > 0`（缓冲写）时，此调用直接关系日志数据完整性；不使用缓冲时，用于关闭文件句柄并停止轮转 goroutine。

### 行为变更（签名不变，语义已变，必须核对）

- **B1 cacheproxy TTL / refresh offset 的 `<= 0` 语义**：`<= 0` 表示**使用默认值**（过期 24h、空值 1m、逻辑刷新 10m），不再是「0 = 永不过期 / 立即回源」。该约定对 `CacheContext` 与直接调用 `RedisCache.Set` 同样生效。
- **B2 `StringView.IsExpire`**：
  - `Ctime` 为零值时，由 `false` 改为 **`true`（视为已过期）**。
  - offset `<= 0` 时回退默认值。
- **B3 `RedisCache.Get`**：
  - ① client 为 nil：由 `panic` 改为返回 `ErrNilRedis`；
  - ② 空 key：现在返回 `ErrInvalidKey`（原 `len(key) < 0` 判断恒假）；
  - ③ 值不可解析：返回包装了 `ErrCorruptedValue` 的错误（附带 key 与字节数），由上层按未命中自愈。
  - **自定义 `Cache` 实现方需遵守该约定**（接口方法集未变）。
- **B4 `MGet` / `MSet`**：原为 `//TODO` 静默返回 `(nil, nil)` / `nil`，现返回 `ErrUnsupported`。
- **B5 未初始化调用**（`GetHit` / `Set` / `Remove`）：由 `panic("empty cacheProxy")` 改为返回 `ErrNotInitialized`。
- **B6 `cacheproxy.Init`**：原可重复调用（每次覆盖），现改为 `once`（**仅第一次生效**）；签名变为 `Init(rdb, opts ...Option)`，旧调用方式兼容。
- **B7 `GetHit`**：ctx 已取消的调用方立即返回，不再等待同 key 的共享回源。
- **B8 httpclient `Timeout <= 0`**：由**无超时**（`http.Client.Timeout = 0` 不限时）改为 **10s**（长耗时请求会被中断）；Transport 补齐默认值（拨号 30s、TLS 握手 10s、`ForceAttemptHTTP2`）并改为克隆宿主 `http.DefaultTransport`。
- **B9 `DalLog` 为 nil**：原实现无 nil 检查（`c.dalLog.Info` 会 **panic**，等于必须注入），现改为静默不记录，且**不回退全局 DAL 日志**（与 `logger.GetDalLog()` 相互独立）。
- **B10 `GetWithRetry` 错误文本**：ctx 结束时，若 `lastErr` 与 ctx 错误异源则附加 `(last err: ...)`，同源则去重；错误链保留（`errors.Is` 双向可命中）。
- **B11 `rpc.ServiceConfig.DrainTimeout <= 0`**：原无条件执行 `nats.DrainTimeout(v)`（传 0 覆盖库默认，导致 Drain 完全不等待在途请求），现视为未设置、使用 nats 默认 30s（优雅关闭最多多等 30s，另有 6s grace 上限）。
- **B12 rpc 空账号**（`Username` / `Password` 均为空）不再显式传 `nats.UserInfo`，避免覆盖 URL 内嵌凭据。
- **B13 `metrics.MetricWhitelist` 空白名单语义反转**：原 `len == 0` → 放行所有，现 → **404 全拒绝**（含全部条目非法时）；404 响应体由字面 `null` 改为 `{}`。
- **B14 `metrics`**：请求 / 响应大小为 `-1` 时不再记录（不再污染直方图）；处理时间由整数毫秒改为浮点毫秒（微秒换算，含亚毫秒精度）。
- **B15 访问日志（`logger.Ginzap`）**：
  - ① body 可能被 `[filtered len=N]` **占位**（Content-Type 缺失 / 非法、gzip、截断、未识别类型且命中未掩码 password）——依赖日志中 body 原文的排障 / 审计流程会受影响；
  - ② `body_size` 恒为原始 body 长度（截断时按 `Content-Length` 还原，未知时为已读下界）；
  - ③ 自定义 `ZapLogger`（不实现 `levelLogger`）时，**msg 由请求 path 统一为 `http`**。
- **B16 `logger.NewGormLogger` 默认值**：`IgnoreRecordNotFoundError` 由 `false` 改为 **`true`**；GORM Info 级日志由 `Debug` 改为 `Info`；`NewGormLogger(nil)` 由 nil panic 改为安全（Nop）；SQL 日志 caller 归属修正。
- **B17 `response`**：`Extension` 为 nil 时 JSON 由 `null` 改为 `[]`。
- **B18 `util`**：`SliceRemoveDuplicates` 始终返回新切片（len ≤ 1 也拷贝，不再与入参共享底层数组）；`ParseTimestamp` 分级改为整数阈值（14+ 位不再被当作毫秒）、error 返回值恒为 nil（原 out-of-range 分支移除）。

### 新增

- **logger**：`logger.Shutdown()`、`logger.InitLoggerWithConfig(LoggerConfig)`（可配置 JSON 编码 / 目录 / info·error·access·panic 是否写 stdout / Compress / `DisableCaller` / `BufferSize` / `BufferFlushInterval`）、`logger.GetLogger()`、`Infof` / `Warnf` / `Errorf`。
- **cacheproxy**：`cacheproxy.WithFetchTimeout(d)`（默认 5s，`<= 0` 回退）、`cacheproxy.WithLogger`、`ErrNotInitialized`、`ErrCorruptedValue`、`ErrNilRedis`。
- **httpclient**：`httpclient.ErrNilClient`；`PostJson` / `GetWithRetry` 的 nil client 防御（返回错误而非 panic）。
- **metrics**：白名单支持 CIDR 与 IPv6（精确 IP 写法仍可用）。
- **rpc**：`rpc.ServiceConfig.Log *zap.Logger` 字段。
- **工程**：GitHub Actions `go test -race ./...` 门禁（`.github/workflows/race-gate.yml`）。

### 弃用

- `StringView.IsNil` 与 `CheckNil()`：当前不参与判定语义，计划下个主版本移除，勿在新代码中依赖。
- `MissedGetter` / `MissedGetterFunc`：为 MGet / MSet 预留，当前无调用点，计划随批量接口决策一并处理。

### 修复

- **日志敏感信息**：访问日志 body / header / query 脱敏与 panic dump 脱敏的 fail-closed 补完（gzip、截断、Content-Type 缺失 / 非法、未掩码 password 兜底）。
- **缓存**：Redis 坏值按命中失败自愈并告警；`atomic.Pointer` 发布消除初始化竞态；同 key 防重刷新；getter panic 收敛为 error；回源超时可配。
- **logger**：`InitLogger` 幂等（不再重复建句柄 / 泄漏轮转 goroutine）；文件缓冲写；`Shutdown` 生命周期。
- **httpclient**：响应体读取失败的错误携带字节数与错误链；ctx 取消不再丢失最后的业务错误。
- **metrics**：`-1` 尺寸不污染直方图；业务码类型断言加保护（避免 panic）。
- **rpc**：panic 收敛为错误响应；drain 预算显式化；空 Url 提前报错。
- **util**：bcrypt 错误不再被忽略；时间戳分级解析修正。

### 使用前提（新增约定）

- **缓存 key 必须全局唯一**：singleflight 以 key 为合并键，不同业务共用同一 key 会互相拿到对方数据并互相覆盖。
- **多机需 NTP 时钟同步**：逻辑过期依赖写入方 `Ctime` 与读取方本地时钟比较。
- **Init 顺序**：先 `logger.InitLogger()`，再 `cacheproxy.Init` / NATS。
- **`DalLog` 为显式 opt-in**：nil 表示不记录任何 DAL 日志。

### 迁移清单

按优先级执行：

1. 升级 Go 与 lumberjack 依赖。
2. 修改 `GetWithRetry` / `BcryptHash` 调用点。
3. 全库搜索 `ExpiredTime: 0` / `RefreshOffset: 0` 等写法并按新语义核对。
4. 核对日志检索与告警规则（msg 口径、body 可能变占位符）。
5. 核对 `MGet` / `MSet`、`MetricWhitelist` 空列表、`Extension == null`、`DrainTimeout` 的既有用法。
6. 在进程退出路径加入 `logger.Shutdown()`（放在最后一步）。
