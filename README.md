# project-framework

要求 **Go 1.26.8** 或更高（见 `go.mod` 的 `go` 行）。下游在默认 `GOTOOLCHAIN=auto` 下会按该版本选取工具链。

代码审查与修复清单：[docs/code-review.md](docs/code-review.md)（含 Go 1.26.8 升级对照）。

建议 Init 顺序：先 `logger.InitLogger()`，再 `cacheproxy.Init` / NATS。`CacheContext` 的 TTL 与 refresh offset **小于等于 0 表示使用默认值**（过期 24h、空值 1m、逻辑刷新 10m），不是「永不过期」或「KEEPTTL / 立即回源」。访问日志仍打 header/body/query，过大截断为 256KiB。
