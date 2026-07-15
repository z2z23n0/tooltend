# ToolTend

**Bundle lifecycle manager for coding-agent tooling.**

*Keep your coding-agent tooling current.*

ToolTend 是面向 Codex 和 Claude Code 的本地生命周期管理器。它把同一个工具产品的 CLI、Skill、Hook、App、配置和内嵌二进制聚合成一个 Bundle，用同一次 Release、同一个策略和同一笔事务管理；同一份物理安装被多个 Agent 使用时只更新一次。

ToolTend v0.2 不提供扩展市场、在线 recipe、搜索或卸载，也不会在初始化时自动接管任何已有工具。Component/Binding 仍保留为底层发现证据和兼容接口，但不再是自动更新的调度单位。

## 安装

正式版本可以直接从 GitHub Release 安装到 `~/.local/bin`。安装器通过 GitHub HTTPS 获取匹配平台资产，并按 release manifest 校验字节数和 SHA-256：

```bash
curl -fsSL https://raw.githubusercontent.com/z2z23n0/tooltend/main/install.sh | bash
```

从源码安装需要 Go 1.22 或更新版本：

```bash
./scripts/install.sh
```

也可以安装 Go module 开发构建；开发构建不含 release 公钥，不能使用签名自更新：

```bash
go install github.com/z2z23n0/tooltend/cmd/tooltend@latest
```

确认 `$(go env GOPATH)/bin` 或 `~/.local/bin` 已在 `PATH` 中，然后运行：

```bash
tooltend version
tooltend init
```

`tooltend init` 在最终确认前只读取本机状态。确认后会安装 ToolTend 自己的 Hook、shim 和 scheduler，扫描并聚合 Bundle，但不会迁移 runtime、执行外部安装器或排队接管任务。所有发现的 Bundle 初始状态都是 `unconfigured`。

已有 ToolTend 状态需要完全重建时，先预览再确认：

```bash
tooltend init --reset-state --dry-run
tooltend init --reset-state --yes
```

重置前会获取全局锁、检查 managed 对象和未完成 journal，并把 config、state、database、data 与基础设施状态备份到相邻的 `tooltend-backups/<timestamp>/`。任一步失败会恢复旧状态和 scheduler。

## Bundle 模型

```text
Bundle
  ├─ BundleRelease：一次整体、精确解析的版本
  ├─ BundleArtifact：CLI / Skill / Hook / App / Config / Binary
  ├─ Installation：唯一物理安装实例
  ├─ ConsumerBinding：Codex / Claude / 项目如何消费该实例
  └─ Policy / Transaction / Receipt / Health Check
```

生命周期所有者固定为：

| 所有者 | 行为 |
|---|---|
| `tooltend` | ToolTend staging、切换、验证和回滚 |
| `delegated` | 编排 npm、mtskills 或官方安装器，并验证、记录结果 |
| `host-owned` | Codex/Claude 管理；ToolTend 只观察 |
| `app-owned` | App 自带更新器管理；ToolTend 只观察 |
| `workspace-linked` | 链接本地仓库；默认只观察 commit 和健康 |
| `unresolved` | 无法高置信识别；禁止自动更新 |

内置 `bundle-recipe-v1` recipe 随二进制发布。本地扩展放在 `~/.config/tooltend/bundles.d/*.toml`，首次配置必须显式信任。所有命令只能声明静态 argv，不能使用 shell 字符串。

## 工作方式

ToolTend 不运行常驻 daemon：

```text
SessionStart / ToolUse / 每日任务 / 用户命令
                    │
                    ▼
          tooltend hook / kick
       脱敏记录事件并立即返回
                    │
                    ▼
       tooltend reconcile --once
  排他锁 → 恢复 → 扫描/聚合 → Bundle 事务 → 退出
                    │
                    ▼
       Bundle Receipt 与健康状态
```

- Hook 热路径不联网、不合并、不调用模型，SQLite 使用 `busy_timeout=0`；数据库繁忙或输入异常时 fail-open。
- `kick` 只启动一个脱离当前会话的一次性 worker。全局文件锁保证并发 Session 不会并行更新。
- macOS 使用 launchd，Linux 使用 systemd user timer；两者每天启动一次 `reconcile --once`，没有常驻 ToolTend 进程。
- 未执行 `bundles configure` 的 Bundle 不检查更新、不下载，也不调用安装器。
- Bundle 更新先完成所有 Artifact 的解析、校验和 staging，再按物理 Installation 激活；失败时按相反顺序补偿。
- Bundle 事务使用步骤 journal。中断、失败、回滚和健康检查都有 Bundle 级 Receipt 可审计。

## 策略

每个 Bundle 有一个明确策略：

```toml
mode = "auto" # auto | manual | observe | ignore
```

- `auto`：仅允许 recipe 同时具备精确解析、完整 staging、激活、健康检查和可靠补偿回滚。
- `manual`：允许检查更新，但每次整包应用都需要用户确认。
- `observe`：只记录版本、来源、漂移和健康，不执行替换命令。
- `ignore`：保留发现证据，不检查更新。

`host-owned`、`app-owned`、`workspace-linked` 和 `unresolved` 只能选择 `observe` 或 `ignore`。交互配置中回车表示跳过，未选择的 Bundle 始终保持 `unconfigured`。

## 更新与回滚

每次 Bundle 更新严格依次经过：

```text
解析整体 BundleRelease 和精确 Artifact 版本
→ 全部下载、完整性校验和 staging
→ 兼容性、权限、Hook 和本地修改检查
→ 每个物理 Installation 只更新一次
→ 派生步骤和 Bundle/Artifact 健康检查
→ 提交 Receipt；失败则反向补偿
```

delegated driver 复用用户现有认证环境，但不会保存或输出 registry token、环境变量或完整命令。默认 resolve 超时 30 秒、安装 5 分钟、健康检查 30 秒，最多重试 3 次。

## 发现证据

发现优先读取 npm `package.json`、Git commit、GitHub Release、本机 `.agents/.skill-lock.json`、mtskills 来源记录和签名 manifest、App `Info.plist`/代码签名/Sparkle，以及仓库链接。Skill 文档里的 `latest`、示例命令和依赖约束只作为需求证据，绝不会当作已安装版本。

Codex 插件缓存由 Host 管理，ToolTend 聚合为 `host-owned` 观察对象，不参与下载或替换。无法高置信聚合的对象以 fallback Bundle 保留，`bundles list --all` 才展示。

## CLI

```text
tooltend init
tooltend init --reset-state --dry-run|--yes
tooltend scan
tooltend status
tooltend bundles list [--all]
tooltend bundles show <bundle>
tooltend bundles configure [--set <bundle>=auto|manual|observe|ignore]
tooltend bundles update [<bundle> | --all] [--stage-only]
tooltend bundles rollback <bundle> [--to <release-or-receipt>]
tooltend bundles history [<bundle>]
tooltend bundles doctor [<bundle>]
tooltend components list
tooltend components show <component>
tooltend policy set <component>
tooltend update [component | --all]
tooltend review [component]
tooltend history [component]
tooltend rollback <component> [--to <receipt-or-version>]
tooltend adopt <component> --source <source> [--subdir <git-relative-path>]
tooltend project init|export|sync
tooltend self status|update
tooltend doctor [--repair]
```

所有命令支持 `--json`；写操作支持 `--dry-run`。在非交互或 JSON 模式中，未提供 `--yes` 的写操作返回 `confirmation_required` 和完整预览，不会提示或偷偷写入。

`components`、`policy`、`adopt`、单组件 `update/rollback/history/review` 是 v0.1 兼容入口，会输出弃用提示。它们不再决定用户看到的 Bundle 数量或 Bundle 更新状态。

JSON 输出使用稳定的 V1 envelope：

```json
{
  "schema_version": 1,
  "command": "status",
  "ok": true,
  "data": {},
  "warnings": []
}
```

Agent 提交 review 时必须同时给出 candidate ID、candidate hash、`safe|conflict|uncertain` verdict、风险类型和摘要；旧候选或 hash 不匹配的判断无效。

## 项目复现

```bash
tooltend project init
tooltend project export
tooltend project sync --dry-run
tooltend project sync --yes
```

- `tooltend.toml` 声明来源、组件类型、目标 Agent 和版本通道。
- `tooltend.lock` 保存 resolved version/commit 与完整性哈希；`project sync` 将其作为 exact 目标和制品哈希约束，并以事务 CAS 应用已确认的预览。
- 两个文件都可以提交到项目仓库；secret、token、来源信任和本机 apply mode 不会写入其中。

## 本地数据

默认遵循 XDG 目录；设置 `TOOLTEND_HOME` 可将全部 ToolTend 数据放到一个独立根目录。

| 内容 | 默认位置 |
|---|---|
| 配置 | `${XDG_CONFIG_HOME:-~/.config}/tooltend/config.toml` |
| SQLite schema v5 状态 | `${XDG_STATE_HOME:-~/.local/state}/tooltend/state.db` |
| activation lock | `${XDG_STATE_HOME:-~/.local/state}/tooltend/activation.lock` |
| objects / staging / generations | `${XDG_DATA_HOME:-~/.local/share}/tooltend/` |
| stable shims | `~/.local/bin/` |
| 本地 Bundle recipe | `${XDG_CONFIG_HOME:-~/.config}/tooltend/bundles.d/*.toml` |
| reset 备份 | config/state/data 相邻的 `tooltend-backups/<timestamp>/` |

SQLite 使用 WAL，schema migration 前创建备份。数据库不保存完整 Prompt、transcript、未经解析的原始命令、环境变量、MCP secret 或 registry token；Hook 只记录标准化 package/version、事件类型和不可逆 correlation hash。

## ToolTend 自更新

正式 release 包含 darwin/linux 的 arm64/amd64 原始可执行文件、`checksums.txt` 和 Ed25519 签名 manifest。安装后的自更新使用二进制内嵌公钥验证签名，并同时检查 release sequence、平台、SHA-256 和字节数。任一不匹配都不会进入 staging；开发构建拒绝自更新。

通过 Homebrew 安装的版本只提示执行对应的 `brew upgrade tooltend`，不会绕过 Homebrew 替换自身。

## 开发

本地与 GitHub Actions 共用同一验证入口：

```bash
./scripts/ci.sh
```

脚本检查 `gofmt`、module 一致性，执行 `go vet ./...`、`go test ./...` 和 `go build ./...`。CI 在 macOS 与 Linux 上运行，以覆盖文件锁、generation 指针和系统调度差异。

## License

[MIT](LICENSE)
