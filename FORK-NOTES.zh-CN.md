# 本 Fork 的改动

上游：[IrineSistiana/mosproxy](https://github.com/IrineSistiana/mosproxy)，分叉基点 `80afb01`。
原则是尽量少改，只修确认的缺陷，方便跟上游同步或回馈 PR。

## 修复

**1. ECS 前缀没进缓存 key** — `app/router/router_utils.go`

```go
- q.ECS2Upstream.Masked().AppendTo(b)
+ b = q.ECS2Upstream.Masked().AppendTo(b)
```

`AppendTo` 遵循 append 惯例会返回新 slice，丢弃返回值等于没写入。结果 ECS 从未进入缓存 key，
所有客户端共享一条缓存、不分子网，随后的长度字节也写错。仅启用 `ecs` 时触发。

**2. 预取单飞标记从不释放** — `app/router/router_handle.go`

`prefetchCtl.Done` 有定义但零调用点。`Reserve` 把 key 放进 map 后再也不删，于是每个 key
一生只能预取一次，之后永远返回 false。启用乐观缓存时后果被放大：记录过期后无法后台刷新，
会持续返回陈旧结果直到乐观窗口耗尽。同时是无界 map，用随机子域可远程撑爆内存。
修法是在预取 goroutine 里 `defer r.prefetchSf.Done(sk)`。

**3. 域名匹配大小写敏感** — `app/router/server_utils.go`

规则加载时做了小写化，查询名却从未规范化，而匹配用 `bytes.Compare`。于是 `WWW.EXAMPLE.COM`
匹配不到任何规则，直接落到默认路由——基于域名的分流静默失效，reject 规则也能靠翻转一个字母绕过。
RFC 4343 要求大小写不敏感比较，且部分解析器会故意随机化大小写（DNS 0x20）防投毒。
在 `parseQuery` 里补 `q.Question.Name.ToLower()`，顺带修掉缓存 key 碎片化
（同一域名原本有 2^labels 个 key，可被用来冲刷缓存）。

**4. 并发限流计数器泄漏** — `app/router/middleware/limit/limit.go`

超限分支在 `defer h.concurrent.Add(-1)` 注册之前就 return，计数器只增不减。突发几次后
计数永久高于上限，之后所有查询被 REFUSED 直到重启。把 `defer` 提到自增之后即可。

## 新增

**查询日志输出 `elapsed`** — `app/router/log.go`

`QueryCtx.Start` 早就在记录却从未出现在日志里，导致下游无法区分"缓存命中"和"一次慢速递归"。
加 `e.Dur("elapsed", time.Since(q.Start))` 后（单位毫秒）：缓存命中约 `0.09`，冷递归约 `800`，
一眼可辨。

**CI 构建静态二进制** — `.github/workflows/release-binaries.yml`

上游只发 Docker 镜像。打 tag 即产出 linux/amd64 与 arm64 静态二进制 + SHA256，
裸机部署不必装 Docker 或 Go。

## 同步上游

```bash
git remote add upstream https://github.com/IrineSistiana/mosproxy.git
git fetch upstream && git rebase upstream/dev
```
