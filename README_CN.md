# Smart Load Balancer

一个 [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) 插件，用负载感知的路由策略替换默认的 auth 调度策略。

## 解决的问题

当有多个客户端 API Key 和多个上游 auth profile（例如 8 个 Codex profile）时，内置调度器倾向于把不同 Key 的并发请求放到**同一个** profile 上。这浪费了其他 profile，降低了有效并发，还会冲掉 prompt 缓存。

## 功能

启用后，每次 auth 选择都会经过本插件，而不是默认调度器：

- **最少连接数分散（least-connections）**——每个请求被路由到滑动窗口内近期被选中次数最少的 profile，不同 Key 的并发请求会自动落到不同的 profile 上。
- **按 Key 粘性（sticky）**——在 profile 健康的前提下，每个客户端 Key 会被固定到同一个 profile，prompt 缓存保持热度；如果该 profile 过载，Key 会自动溢出到负载最低的 profile。
- **确定性平局打破**——多个 profile 同样空闲时，不同 Key 也会选到不同 profile（平局按 Key 身份和 profile id 的哈希决定），同一个 Key 则保持稳定。

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
         strategy: least-connections # 或 round-robin
         sticky: true
         sticky_ttl_seconds: 1800
         window_seconds: 120
         max_inflight_per_profile: 8
   ```
4. 重启 CLIProxyAPI。

## 配置

| 字段 | 类型 | 默认值 | 说明 |
|---|---|---|---|
| `providers` | array | `[]` | 只对这些 provider 做均衡（例如 `codex`），为空表示全部。 |
| `strategy` | enum | `least-connections` | `least-connections` 按负载分散；`round-robin` 按 profile id 轮询。 |
| `sticky` | bool | `true` | 每个 Key 固定到一个健康的 profile（prompt 缓存亲和）。 |
| `sticky_ttl_seconds` | int | `1800` | 空闲粘性绑定保留多久（秒）。 |
| `window_seconds` | int | `120` | 估计各 profile 近期负载的滑动窗口（秒）。 |
| `max_inflight_per_profile` | int | `8` | 粘性 profile 近期被选中超过该次数后溢出到负载最低的 profile。 |

如果没有候选 profile 匹配 `providers`，或主机没有提供候选，插件会放弃本次决策，主机回退到默认调度——本插件永远不会弄坏请求。

## 原理

插件实现 `scheduler.pick` 能力。主机每次提供候选 auth 列表和入站请求头，插件返回选中的 auth id。负载根据插件自身在 `window_seconds` 内的近期选择来估计（自愈设计：没有跨请求的状态会泄漏，重启后从干净状态开始）。

## 开发

```bash
make test    # 均衡核心的单元测试
make vet     # go vet
make build   # 为本机平台构建插件
make dist    # 交叉编译全部 6 个平台产物
make zip VERSION=0.1.0  # 按商店格式打包 zip + checksums.txt
```

## 发布到官方商店

1. 把 `go.mod`、`main.go`、`plugin-registry-entry.json` 中的 `nitansde` 换成真实的 GitHub 用户名，推送到 GitHub。
2. 打 tag 发布：`git tag v0.1.0 && git push origin v0.1.0`。`release` 工作流会自动构建 6 个平台的 zip 和 `checksums.txt` 并挂到 GitHub Release 上。
3. 向 `router-for-me/CLIProxyAPI-Plugins-Store` 提 PR，把 `plugin-registry-entry.json` 的内容加入 `registry.json`。

## License

MIT
