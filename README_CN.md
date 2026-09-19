[English](./README.md) | **简体中文**

# Smart Load Balancer

一个 [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) 插件，用额度感知、负载感知的路由策略替换默认的 auth 调度策略。

## 解决的问题

当有多个客户端 API Key 和多个上游 auth profile（例如 8 个 Codex profile）时，内置调度器倾向于把不同 Key 的并发请求放到**同一个** profile 上。这浪费了其他 profile，降低了有效并发，还会冲掉 prompt 缓存。

## 功能

启用后，每次 auth 选择都会经过本插件，而不是默认调度器：

- **额度感知排序**——按额度状态给 profile 排序：先是你的 `quota_priorities`，然后是长窗口（周/月）reset 最早的，再在每个 reset tier 内 fill-first（精准 `used_percent` 高的优先，其次是账本累计 token 多的）。快到期的额度先用，离到期还远的后用。
- **不冲突保证**——只要还有没被占用的 profile，就绝不会抢走别的 Key 的 sticky profile；不同 Key 的并发请求会自动落到不同的 profile 上。
- **按 Key 粘性**——在 profile 健康的前提下，每个客户端 Key 会被固定到同一个 profile（默认空闲 24 小时，可调），prompt 缓存保持热度；如果该 profile 被 block 或耗尽，Key 会透明地 failover；如果过载，会溢出到负载最低的 profile。
- **上次用过偏好**——sticky TTL 过期后，客户端上次用过的 profile 仍会作为一个弱排序偏好。它永远不会凌驾于不冲突保证和额度排序。
- **Fill-first 平局打破**——在所有排序键上都打平的候选按自然 ID 排序（`profile-2` 排在 `profile-10` 前面），永远取第一个。`strategy` 设置会被接受但忽略。

客户端身份由入站 `Authorization`（或 `X-Api-Key`）头的 SHA-256 哈希派生，原始 Key 内容不会被存储或记录。

## 安装

### 从插件商店安装（推荐）

发布到官方商店后，在 CLIProxyAPI 管理界面或 CLI 中搜索 `smart-load-balancer` 安装。

### 手动安装

1. 为你的平台构建动态库：
   ```bash
   make build
   # 生成 smart-load-balancer.so（macOS 为 .dylib，Windows 为 .dll）
   ```
2. 复制到 CLIProxyAPI 的 `plugins` 目录。
3. 在 `config.yaml` 中添加：
   ```yaml
   plugins:
     enabled: true
     configs:
       smart-load-balancer:
         enabled: true
         priority: 1
         providers: ["codex"]      # 可选：只对这些 provider 做均衡；为空表示全部
         strategy: least-connections # 为兼容而接受，实际被忽略
         sticky: true
         sticky_ttl_seconds: 86400  # 24h（默认值）
         window_seconds: 120
         max_inflight_per_profile: 8
         quota_enabled: true        # 额度感知排序，靠用量反馈（无需轮询）
         quota_providers: ["codex"] # 有已知额度接口的 provider；为空表示全部已知
         quota_refresh_seconds: 0   # 0 = 关闭后台校准（仅按需/手动）；默认 72h
         quota_probe_fresh: true    # 首次选中从未用过的 profile 时发一次最小 ping，启动它的周窗口计时
         quota_priorities: []       # 按偏好排序的 auth profile ID，最优先的在前；没列出的排最后
   ```
4. 重启 CLIProxyAPI。

## 配置

| 字段 | 类型 | 默认值 | 说明 |
|---|---|---|---|
| `providers` | array | `[]` | 只对这些 provider 做均衡（例如 `codex`），为空表示主机提供的全部。 |
| `strategy` | enum | `least-connections` | 为兼容而接受（`least-connections` / `round-robin`），但实际被忽略：插件永远取额度感知排序的第一名。 |
| `sticky` | bool | `true` | 每个 Key 固定到一个健康的 profile（prompt 缓存亲和）。 |
| `sticky_ttl_seconds` | int | `86400`（24h） | 空闲的粘性绑定保留多久（秒），管理界面可调。 |
| `window_seconds` | int | `120` | 估计各 profile 近期负载的滑动窗口（秒）。 |
| `max_inflight_per_profile` | int | `8` | 粘性 profile 近期被选中超过该次数后溢出到负载最低的 profile。 |
| `quota_enabled` | bool | `true` | 额度感知排序。主要信号是用量反馈账本（见下）；精准的上游快照只用于校准。 |
| `quota_providers` | array | `[]` | 只对这些 provider 取精准额度。为空表示所有有已知额度接口的 provider。 |
| `quota_refresh_seconds` | int | `259200`（72h） | 后台精准额度校准间隔。`0` 表示完全关闭后台校准：只在按需时刷新（管理界面手动刷新，会走本插件的 `quota.fetch`）。刻意设得很低频，避免上游限流风险。 |
| `quota_probe_fresh` | bool | `true` | 首次选中从未用过的 profile 时，发一次最小 ping 来启动它的周窗口计时。 |
| `quota_priorities` | array | `[]` | 按偏好排序的 auth profile ID，最优先的在前。在额度感知排序内部生效；没列出的排最后。 |

如果没有候选 profile 匹配 `providers`，或主机没有提供候选，插件会放弃本次决策，主机回退到默认调度——本插件永远不会弄坏请求。

## 额度感知调度的工作原理

插件注册了三个能力：`scheduler`、`usage_plugin` 和 `quota_provider`。

**用量反馈是主要的额度信号——无需轮询。** 每次请求结束后，主机都会调 `usage.handle` 送来一条用量记录。插件维护每个 profile 的账本：成功的请求累计消耗的 token，上游 `429` 按类型处理：

- 明确的周/月额度耗尽信号：block 到窗口结束；
- 没指明窗口的额度失败：退避 5 小时再试；
- 明确的瞬时限流（例如并发超限）：不 block。

没经过本插件调度过的 profile 默认按 100% 剩余处理；路由都由本插件负责，所以它自己的账本是权威的，漂移靠校准纠正。

**额度感知排序。** 每次选择先排除被 block 的 profile。客户端的 sticky profile 在健康时会被保留（prompt 缓存亲和，默认空闲 24h）；被 block 或耗尽后自动 failover。Fresh 选择按以下顺序给 profile 排名：你的 `quota_priorities` 最先；然后被别的 Key 占用的 profile 排在所有没被占用的之后（不冲突保证：只要还有没被占用的就绝不抢；只有全部被占用时才按占用数最少的选）；然后已知额度的排在未知额度之前；然后长窗口 reset 最早的优先（长窗口是周或月，看账号类型；reset 时间差 1 小时内算同一时刻；没有已知 reset 的排在有 reset 的之后）；然后在每个 reset tier 内 fill-first（精准 `used_percent` 高的优先，其次账本累计 token 多的，从未用过的最后）；然后未知额度的按 host priority tier（数字大的优先，和 CPA 默认调度器一致）；然后近期负载最少的；然后客户端上次用过的 profile（纯弱偏好，绝不凌驾于不冲突保证和额度排序）；最后自然 ID 排序取第一个（fill-first）。`strategy` 设置会被接受但忽略。

**精准额度是按需校准。** 插件实现了 `quota_provider` 能力，所以你在管理界面手动刷新某个凭证的额度时，主机调的是本插件的 `quota.fetch`——顺手就更新了调度器的快照，一套代码，没有重复劳动。后台校准默认 72 小时一次，也可以整个关掉（`quota_refresh_seconds: 0`），只用手动刷新。每次校准周期会把各 profile 的拉取分散在 6 小时窗口内（按 profile 数量定间隔），不会同时打上游接口。每次请求还会顺手被动采集额度：主机把上游的额度事件合并进 `usage.handle` 的响应头里，所以每个请求都自带它 profile 最新的窗口数字，零额外请求。活跃的 profile 光靠流量就能保持校准。

独立于后台周期，每次选择时如果发现某个 profile 的长窗口（周/月）reset 时间已过，插件会在后台重新拉取该 profile 的精准额度（每个 profile 单 flight，不同 profile 之间至少间隔 10 分钟），而不是相信过期的快照。任何路径拉取失败，该 profile 冷却 5 小时后才允许各路径重试。5 小时窗口不单独重拉：靠本地用量反馈估算，估错了会变成一次失败请求，账本会把它转成 5 小时 block。

## 原理

插件实现 `scheduler.pick` 能力。主机每次提供候选 auth 列表（含 host priority tier 和状态）和入站请求头，插件返回选中的 auth id。负载根据插件自身在 `window_seconds` 内的近期选择来估计（自愈设计：没有跨请求的状态会泄漏，重启后从干净状态开始）。

## 开发

```bash
make test    # 均衡核心的单元测试
make vet     # go vet
make build   # 为本机平台构建插件
make dist    # 交叉编译全部 6 个平台产物
make zip VERSION=0.1.0  # 按商店格式打包 zip + checksums.txt
```

端到端检查会把编译好的 `.so` 走一遍真实 C ABI（`plugin.register` → `scheduler.pick` → `plugin.reconfigure`），见开发时用的测试脚本。

## 发布到官方商店

1. 把 `go.mod`、`main.go`、`plugin-registry-entry.json` 中的 `nitansde` 换成真实的 GitHub 用户名，推送到 GitHub。
2. 打 tag 发布：`git tag v0.1.0 && git push origin v0.1.0`。`release` 工作流会自动构建 6 个平台的 zip 和 `checksums.txt` 并挂到 GitHub Release 上。
3. 向 `router-for-me/CLIProxyAPI-Plugins-Store` 提 PR，把 `plugin-registry-entry.json` 的内容加入 `registry.json`。

## License

MIT
