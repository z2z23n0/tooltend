# ToolTend

**Lifecycle manager for coding-agent extensions.**

*Keep your coding-agent tooling current.*

ToolTend 是面向 Codex 和 Claude Code 的本地生命周期管理器。它统一盘点 Skill、Plugin、Hook、stdio MCP Server，以及这些扩展明确依赖的专用 CLI；在可验证、可回滚的边界内检查更新、保留本地修改、原子切换版本，并在失败时继续使用旧版本。

ToolTend V1 不提供扩展市场、发布、搜索或卸载，也不接管通用 CLI、项目依赖、远程 HTTP MCP 的版本和整台机器。

## 安装

需要 Go 1.22 或更新版本。可以从源码安装到 `~/.local/bin`：

```bash
./scripts/install.sh
```

也可以直接安装 Go module：

```bash
go install github.com/z2z23n0/tooltend/cmd/tooltend@latest
```

确认 `$(go env GOPATH)/bin` 或 `~/.local/bin` 已在 `PATH` 中，然后运行：

```bash
tooltend version
tooltend init
```

`tooltend init` 在最终确认前只读取本机状态。它只扫描 Codex、Claude Code 的官方配置层、已知扩展目录和用户选择的项目，不遍历整个 home。统一预览会列出状态目录、Host Hook、每日任务、runtime shim 和配置变更；交互确认一次后才写入，并为已确认的精确 npm/Python runtime 异步排队安全迁移。

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
  排他锁 → 恢复 → 扫描 → 检查 → staging → 退出
                    │
                    ▼
       generation 或 shim 原子切换
```

- Hook 热路径不联网、不合并、不调用模型，SQLite 使用 `busy_timeout=0`；数据库繁忙或输入异常时 fail-open。
- `kick` 只启动一个脱离当前会话的一次性 worker。全局文件锁保证并发 Session 不会并行更新。
- macOS 使用 launchd，Linux 使用 systemd user timer；两者每天启动一次 `reconcile --once`，没有常驻 ToolTend 进程。
- SQLite 和文件系统之间使用 activation intent journal。进程中断后，下一次 worker 会按真实 generation 指针完成提交或回滚。
- 正常更新、回滚和 adopt 都生成 Receipt，可供 `history` 审计。

## 策略

每个 Binding 有独立策略：

```toml
track_channel = "stable" # stable | latest | main | semver | exact
constraint = "^1.6"
apply_mode = "auto"      # auto | manual | ignore
notify_mode = "failures" # all | failures | none
```

- `auto`：只有来源明确、Binding 已 adopt、验证通过且具有可靠回滚边界时，后台 staging 并激活。
- `manual`：后台只 resolve 并记录可用更新；主动运行 `tooltend update` 后才下载和应用。
- `ignore`：保留在 inventory 中，不联网检查，也不更新。
- `exact` 是固定版本通道，不另设 `pin`。
- 本地 policy 是权限上限。项目 manifest 可以声明版本目标，但不能授予来源信任，也不能把本机的 `manual`、`ignore` 或 `exact` 放宽。

无 Baseline 的已有副本按 Fork 处理并保持 `manual`，不会伪造三方合并。Hook 内容、执行权限、Host 信任哈希或来源身份发生变化时，旧版本继续生效，候选进入 `needs_review`。

## 更新与回滚

文件型组件在 adopt 后进入 ToolTend generation 目录；原安装位置成为稳定指针。每次候选依次经过：

```text
resolve → staging → 来源/完整性验证
→ Baseline + Binding Overlay + 新上游三方合并
→ 确定性验证 → 必要的 candidate-bound review
→ intent journal → 原子指针切换 → 健康检查 → Receipt
```

本地修改以 Binding Overlay 保存，因此相同组件在不同 Agent 或项目中的定制互不覆盖。文本冲突、验证失败、审查结果为 `conflict`/`uncertain` 或 candidate hash 不匹配时，不切换当前版本。

runtime 组件采用隔离环境和稳定 shim。npm、pipx/uv 的原生全局安装在 adopt 前保持 `manual`；迁移先重建并验证精确版本，不自动卸载原安装。

## 适配器边界

| 适配器 | 发现/检查 | 自动更新边界 | 回滚 |
|---|---|---|---|
| Git Skill / Plugin / Hook | 支持 | managed、来源和 Baseline 明确、确定性验证通过；Hook 信任变化除外 | generation 指针 |
| npm CLI / stdio MCP | 支持 | adopt 到隔离 prefix 和稳定 shim 后 | 精确版本 generation |
| npx package 引用 | 解析 package | adopt 为固定 runtime 或 wrapper 后 | 恢复旧 wrapper |
| pipx / uv tool | 支持 | adopt 到隔离环境和 shim 后 | 精确版本环境 |
| uvx package 引用 | 观察 | adopt 为持久 runtime 后 | 恢复旧 runtime |
| Homebrew | 发现和检查 | 默认 `manual`；只有降级路径预验证后才可能自动 | 原生精确降级 |
| 远程 HTTP MCP | 配置与可用性观察 | 不做版本更新 | 不适用 |
| 未识别安装器 | 观察 | 不猜测升级命令 | 不适用 |

`git`、`gh`、`node`、`npx`、`bash` 等载体不会因为普通命令调用被纳入 inventory。

## CLI

```text
tooltend init
tooltend scan
tooltend status
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
| SQLite 状态 | `${XDG_STATE_HOME:-~/.local/state}/tooltend/state.db` |
| activation lock | `${XDG_STATE_HOME:-~/.local/state}/tooltend/activation.lock` |
| objects / staging / generations | `${XDG_DATA_HOME:-~/.local/share}/tooltend/` |
| stable shims | `~/.local/bin/` |

SQLite 使用 WAL，schema migration 前创建备份。数据库不保存完整 Prompt、transcript、未经解析的原始命令、环境变量、MCP secret 或 registry token；Hook 只记录标准化 package/version、事件类型和不可逆 correlation hash。

## ToolTend 自更新

正式 release 的自更新 manifest 使用嵌入式 Ed25519 公钥验证，平台 asset 同时校验签名 manifest 中的 SHA-256 和字节数。签名、序列号、平台或完整性任一不匹配都不会进入 staging。没有嵌入 release key 的开发构建拒绝自更新。

通过 Homebrew 安装的版本只提示执行对应的 `brew upgrade tooltend`，不会绕过 Homebrew 替换自身。

## 开发

本地与 GitHub Actions 共用同一验证入口：

```bash
./scripts/ci.sh
```

脚本检查 `gofmt`、module 一致性，执行 `go vet ./...`、`go test ./...` 和 `go build ./...`。CI 在 macOS 与 Linux 上运行，以覆盖文件锁、generation 指针和系统调度差异。

## License

[MIT](LICENSE)
