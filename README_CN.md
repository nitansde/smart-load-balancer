[English](./README.md) | **简体中文**

# Smart Load Balancer

一个 [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) 插件：把每个客户端 API Key 路由到合适的上游 profile——快到期的额度先花，每个 Key 固定在自己的 profile 上让 prompt 缓存保持热度，额度用完的 profile 自动跳过。

## 30 秒讲清它解决什么问题

假设你有 3 个 API Key（笔记本、家里服务器、CI 任务），背后是 3 个 Codex 账号：

```
不用这个插件                      用了这个插件

key 笔记本 ──┐                    key 笔记本 ──▶ profile 1（固定）
key 服务器 ──┼──▶ profile 1       key 服务器 ──▶ profile 2（固定）
key CI     ──┘    （挤爆了！）    key CI     ──▶ profile 3（固定）
             profile 2、3 空着
```

内置调度器容易把并发的 Key 全堆到同一个 profile 上：其他 profile 空转，有效并发被单个账号的限额卡死，不同 Key 的对话还会互相冲掉 prompt 缓存。

这个插件把每个 Key 固定到自己的 profile 上，profile 健康就一直用；额度用完或出错，Key 自己搬家，不用你手动折腾。

## 它怎么选 profile

```
key K 的请求来了
        │
        ▼
K 已经有固定的 profile 了吗？
  │ 有                      │ 没有
  ▼                         ▼
它还健康吗？                给所有 profile 打分排名，第一名获胜：
  │ 健康    │ 不健康        1. 你的 quota_priorities（VIP 名单）
  ▼         ▼               2. 没被别的 Key 占用的优先
继续用   换一个             3. 额度已知的优先于未知的
（缓存保持热度）            4. reset 最早的先用——先花快到期的额度
                            5. 用得最多的先用——到期前把它填满
                            6. host 优先级、负载、上次用过、ID 顺序……
                          把获胜者记为 K 的固定 profile
```

一句话：快到期的额度先花，别人正在用的 profile 不抢（有空闲的话），每个 Key 固定在一个 profile 上让 prompt 缓存保持热度。

## 额度数字从哪来

没有高频轮询，额度从三个地方来：

```
每次请求 ──▶ usage.handle ──▶ 账本：花了多少 token、429 封禁
             │
             └──▶ 响应头：精准 used% + reset 时间
                  （上游额度事件顺手捎带，零额外请求）

你在管理界面点"刷新" ──▶ quota.fetch ──▶ 上游 ──▶ 快照库
                         （只在你点的时候拉）

快照过期或全新账号 ──▶ 打上待校准标记 ──▶ 下一次合适的真实
                                          请求被借道一次，响应头刷新快照。
                                          没有真实流量就不校准——插件永远
                                          不会自己伪造请求。
```

上游返回的 `429` 会分类处理，而不是无脑重试：明确的周/月额度耗尽就 block 到窗口结束；没指明窗口的额度失败退避 5 小时；明确的瞬时限流（比如并发超限）完全不 block。

## 安装

### 方案 A —— 插件商店（推荐，不用自己编译）

在 `config.yaml` 里加上本插件的 registry，重启 CLIProxyAPI，然后在管理界面的插件商店里安装 `smart-load-balancer`：

```yaml
plugins:
  store-sources:
    - "https://raw.githubusercontent.com/nitansde/smart-load-balancer/main/registry.json"
```

### 方案 B —— 手动编译

1. `make build` → 得到 `smart-load-balancer.so`（macOS 是 `.dylib`，Windows 是 `.dll`）
2. 复制到 CLIProxyAPI 的 `plugins` 目录
3. 在 `config.yaml` 里添加：
   ```yaml
   plugins:
     enabled: true
     configs:
       smart-load-balancer:
         enabled: true
         sticky_ttl_seconds: 86400  # 默认 24h
   ```
4. 重启 CLIProxyAPI

## 配置

管理界面露出三个选项，其他都用内置默认值：

| 字段 | 默认值 | 说明 |
|---|---|---|
| `enabled` | `true` | 总开关。关闭 = 插件让位，主机用默认调度器。 |
| `sticky_ttl_seconds` | `86400`（24h） | 空闲的 Key 在它的 profile 上固定多久。 |
| `five_hour_boost` | `false` | 5h 加速模式。打开后，5h 额度 100% 且倒计时没在跑时，会借一条真实小请求消耗一点 token 来启动倒计时——和周额度一样的 cherry-pick 机制，插件本身不主动发任何请求。 |

<details>
<summary>高级参数（只有手改 config.yaml 才需要）</summary>

`providers`、`strategy`（接受但忽略）、`sticky`、`window_seconds`（120）、`max_inflight_per_profile`（8）、`quota_priorities`。

</details>

## 细节

- **客户端身份**是入站 `Authorization`（或 `X-Api-Key`）头的 SHA-256 哈希，原始 Key 不会存储也不会打日志。
- **永远不会弄坏请求。** 没有候选 profile、或插件被关闭时，它会放弃本次决策，主机回退到默认调度器。
- **`strategy` 设置**为了兼容会被接受，但实际被忽略——永远按上面的排名规则选。
- **校准过期或全新的账号。** 插件永远不发探测请求。账号快照过期（长窗口 reset 已过）或账号全新时，会被打上待校准标记，下一次合适的真实请求会被借道一次——优先借平均 token 较小 key 的请求（每个 key 只统计最近 100 次），5 分钟内等不到小请求就借任意请求。借道不改变 key 的 sticky 绑定，借了但没学到东西（非额度失败）会冷却 5 分钟再试。
- **精确的排名规则。** Fresh 选择按以下顺序排名：(1) 你的 `quota_priorities`；(2) 被别的 Key 占用的 profile 排在没被占用的之后（只有全部被占用时才按占用数最少的选）；(3) 额度已知的排在未知之前；(4) 长窗口（周/月）reset 最早的优先——reset 时间差 1 小时内算同一时刻，没有已知 reset 的排最后；(5) 同一 tier 内 fill-first：精准 `used_percent` 高的优先，其次账本累计 token 多的，从没用过的最后；(6) 未知额度的按 host priority（数字大的优先）；(7) 近期负载最少的；(8) 这个 Key 上次用过的 profile（纯弱偏好）；(9) 自然 ID 排序，取第一个。

## 开发

```bash
make test    # 单元测试
make vet     # go vet
make build   # 给本机编译
make dist    # 交叉编译全部 6 个平台
make zip VERSION=0.1.0  # 按商店格式打包 zip + checksums.txt
```

## License

MIT
