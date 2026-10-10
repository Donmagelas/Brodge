# Desktop / IM Bridge Development

1. 如无必要，勿增实体。
2. 客观地质疑用户，指出方案中的问题。

当前实现基于 cc-connect 二开，连接飞书和本机现有 Codex Desktop 会话。
微信等 IM 是后续方向，未要求时不实现。保留用户现有 Desktop 登录、API、网关、模型和权限设置；不读取或复制 Codex auth，不切换到 CLI 或独立 app-server。

依赖方向：`cmd/brodge → config, core, agent, platform`；`agent` 和 `platform` 只依赖 `core`，`core` 不导入具体 adapter。保留必要 IM 接口，不增加通用 agent 生命周期、服务商、技能管理或用户定时任务。

消息必须经过认证来源校验和 SQLite 持久入口；不能用内存去重、后台 fire-and-forget 或时间戳丢弃替代。恢复不确定提交时只核对 Desktop 回合证据，不重复执行。回复保持原来源和固定 UUID。管理接口只绑定 loopback，要求 token。不要输出凭据或原始 SDK 消息日志。

新增行为和 bug 修复要有回归测试。多步骤会话行为在 `agent/codexdesktop/desktop_test.go` 的 `TestCUJ` 验证，使用实际 SQLite、模拟 Desktop Unix socket 和 IM 边界。改动后运行 `go test ./...`、必要的 `go test -race ./...`、`go vet ./...`、`npm run build --prefix web`，并检查 diff。真实外部联调只使用用户明确授权的可丢弃会话和 IM 入口；测试通过结论不能覆盖未验证能力。
