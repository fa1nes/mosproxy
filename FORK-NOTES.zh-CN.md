# 本 Fork 的改动

上游：[IrineSistiana/mosproxy](https://github.com/IrineSistiana/mosproxy)，分叉基点 `80afb01`。
本仓库独立维护。改动仍然保持小而独立、每个缺陷一个提交，便于日后回溯与合并上游更新。

下面每条都经过实际验证，不是静态推断。

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

**5. 截断没有置 TC 标志，段计数也不修正** — `pkg/dnsmsg/msg.go`

`Pack` 把 header 复制到 `msgHdr` 供截断逻辑设置 TC，但真正写入线缆的 bits 取自原始
`m.Header`，那个标志被丢弃了；各段计数同样是打包前算好、之后从不因跳过的记录而调整。
于是客户端拿到一个被削短的应答，却看到 TC=0 和一个与实际内容不符的 ANCOUNT——
它无从得知记录缺失，也就永远不会改用 TCP 重试，把残缺应答当成完整结果。
附带的回归测试 `pkg/dnsmsg/truncation_test.go` 会构造 80 条 A 记录压到 512 字节上限：
修复前是"裁到 17 条但 TC=false"，修复后为"裁到 17 条且 TC=true"。

**6. Commit/Discard 抹掉了已生效的校验和** — `app/router/data_loader.go`

`Commit` 把新校验和存进 `s.hash` 后立刻又将其清零（本意是清 staging 的那份），
`Discard` 同理清错了对象。结果文件**第一次真正变化之后**，记录的校验和恒为零、永不匹配，
此后每次 reload 都会重新读取并解析文件，哪怕内容一个字节都没变。
实测：连续两次 reload 相同内容，修复前两次都打印 `file loaded`，修复后第二次正确地
`skip loading file, same checksum`。

**7. 路径前缀匹配的长度判断写成了自比较** — `app/router/rule.go`

`len(q.Path) >= len(q.Path)` 恒为真，导致后面的切片在请求路径短于前缀时越界 panic，
直接带崩进程。只要配置了以 `/` 结尾的 `path:` 规则，再来一个更短的（或来自 UDP/TCP
服务器因而为空的）路径即可触发。

**8. base64 解码长度被丢弃，池内残留数据混入查询** — 两个 HTTP 服务端

`DecodedLen` 只是上界，解码器会跳过换行等字符，实际写入可能更短。原代码丢弃了返回值、
把整个缓冲区当作查询报文传下去，于是**上一个请求遗留在池化缓冲区里的字节**被拼到了
本次查询尾部；若报文中的压缩指针指向那段，这些残留会被当成域名解析，并原样回显在
应答的 question section 里。改为按实际解码长度切片。

**9. 缓存条目没有按转发路由隔离** — `app/router/router_utils.go`、`rule.go`

缓存 key 只由查询本身（名字/类型/ECS）构成，不含命中的 `forward` 规则。两条规则把同一个
域名转给不同上游时，它们共用同一条缓存：先到的那个上游的应答会被另一条路由的客户端拿到。
对按地域分流的部署这是直接的正确性问题——境外线路可能读到境内上游写进去的应答，反之亦然，
而且完全不报错。修法是把命中的 forward tag 一并写进 key。

**10. `fall_through` 后端在单次查询内不重试** — `app/router/upstream_lb.go`

`fall_through` 的语义是"这个后端不行就换下一个"，但传输错误与 SERVFAIL 只会影响**后续**
查询的选择，当次查询直接把失败结果返回给客户端。于是每个后端出问题时都要先赔上一次可见的
解析失败。改为在同一次查询内就向下一个后端重试。

**11. ECS 被过度抑制** — `app/router/ecs.go`、`router_handle.go`

配了 `ecs.ip_zone` 时，凡是 zone 文件没标记的地址都被当作"本地客户端"而清掉 ECS。但 zone
文件是从归属库生成的，只覆盖该库标注过的空间——本部署实测**路由快照里 69.85% 的大陆 IPv4
空间没有标记**，这些客户端的 ECS 被整个丢弃，上游只能按解析器位置作答，恰好是 ECS 要避免
的事，而且没有任何地方报告。`appendCacheKey` 本来就能处理"没有 zone 名"（退化成按原始前缀
分片），也就是说这个特性的两半自相矛盾：缓存准备好了按 /24 分片，处理器却先把前缀毁掉了。
改为只对**确实不携带任何上游可用地域信息**的地址抑制——回环、私网、CGNAT、链路本地、
benchmarking、文档与组播。局域网和隧道客户端行为不变，公网客户端无论 zone 文件有没有点名
它的网段，都能拿到按子网作答的结果。

## 新增

**查询日志输出 `elapsed`** — `app/router/log.go`

`QueryCtx.Start` 早就在记录却从未出现在日志里，导致下游无法区分"缓存命中"和"一次慢速递归"。
加 `e.Dur("elapsed", time.Since(q.Start))` 后（单位毫秒）：缓存命中约 `0.09`，冷递归约 `800`，
一眼可辨。

**TLS 证书热重载** — `app/router/tls.go`

证书只在启动时读一次，续期后唯一的生效方式是重启——对 DNS 服务器意味着掐断所有在途的
DoH/DoT 连接、丢掉整个缓存、重置 TLS session ticket key。用短效证书时（Let's Encrypt 的
IP 证书约 6 天）这笔开销每隔几天就要付一次。改为经 `GetCertificate` 提供证书，按 mtime
变化惰性重载，并限速到每 10 秒一次，让握手路径上不出现 stat。**重载失败时保留旧证书**：
ACME 客户端不会原子地同时写入证书和私钥，在那个窗口里握手失败比继续用一张仍然有效的旧证书
更糟。

**剥离 SVCB/HTTPS 应答里的 `ech` SvcParam** — `pkg/dnsmsg/svcb.go`

Cloudflare 正在铺开 Encrypted Client Hello。客户端一旦从 HTTPS 记录里读到 `ech` 参数，
就会加密真实 SNI 并在明文 ClientHello 里放一个掩护名（`cloudflare-ech.com`）。所有按域名
分流的代理从此看到的都是掩护名，域名规则**静默失配**、流量落到默认出口，而 DNS 解析、TLS
握手和页面加载全都照常成功。`RuleConfig` 只能按 domain/server/path/client_ip 匹配，配置层
无解，只能在报文层处理。HTTPS(65) 在 `unpackResource` 里走 `RawResource`、保留线格式 RDATA，
因此直接在原始字节上做参数摘除；SvcParams 是升序 TLV 列表，删掉一项仍然有序，可以零分配就地
压缩。**只移除 key 5，不丢弃整条记录**——丢掉会连 `alpn="h3"`（没有 HTTP/3）和
`ipv4hint`/`ipv6hint` 一起失去。

**CI 构建静态二进制** — `.github/workflows/release-binaries.yml`

上游只发 Docker 镜像。打 tag 即产出 linux/amd64 与 arm64 静态二进制 + SHA256 + build-id，
裸机部署不必装 Docker 或 Go。`build-id` 供部署脚本核对拿到的产物确实是这个 tag 构建的。

## 合并上游更新

```bash
git remote add upstream https://github.com/IrineSistiana/mosproxy.git
git fetch upstream && git rebase upstream/dev
```

上游是 alpha 阶段、无 tag/release 的项目，更新节奏不稳定。合并后请至少重跑
`go test ./...` 与 `go test -race ./app/router/... ./pkg/dnsmsg/...`，
其中 `truncation_test.go` 能直接兜住第 5 条被上游改动覆盖的情况。
