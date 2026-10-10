# Brodge

在飞书中继续本机 Codex Desktop 会话。基于 cc-connect `dfad19415a38b00b2c5c288610784d1a7eef337f` 二开，保留上游来源与归属，Go module 暂沿用上游路径。

只允许飞书应用创建者使用：可以私聊，也可以在任意已加入机器人的群内真实 @ 它，不设群白名单。不同入口独立绑定；`/bind codex://threads/...` 导入会话，`/list`、`/switch` 只操作该入口的关联会话，`/current` 查询当前绑定。收到消息加表情，任务结束后发送最终答案；图片和附件通过飞书官方 API 收发。已真实验证当前 Desktop 异步弹窗的选项和自由回答；问题须在原回合仍有效时作答。

Codex 的登录、API／网关、模型、技能和权限继续由 Desktop 管理。微信、Windows、Web 外观改版、定时任务和服务商管理不在当前范围。无绑定首条消息的创建采用 macOS 辅助功能操作 Desktop 界面，创建无项目会话后通过 IPC 接续原首轮；代码及模拟链路已通过，真实界面尚待验收。仅 `/new` 暂缓。未加载会话连接失败时会尝试通过 Desktop 会话链接自动打开并重连，可能使 Desktop 显示该会话。

```sh
make build
./Brodge --config config.toml
make app
```

`build/Brodge.app` 可双击，后台与 Web 一起启动并打开普通浏览器。直接关闭网页不会停止后台；Web 的“关闭 Brodge”按钮停止后台，浏览器允许时同时关闭标签，否则显示已退出。重复双击会复用后台并重开管理页。没有登录自启动或额外后台服务安装。

本机沿用现有配置打包：`./scripts/package-macos.sh /绝对路径/config.toml`。该包只保存配置文件路径，不把应用凭据打进包内。首次读取文稿目录中的已有配置时，macOS 可能请求文件访问权限。

首次自动创建需要在系统设置中允许 Brodge 的辅助功能权限，当前适配 Desktop 26.928.21956（12404）。明确尚未发送的失败可以在 Web 重试；发送结果不确定时，填写实际创建的会话链接核对后接续，不自动重发首条消息。已有绑定继续使用 IPC。

不带参数时，Brodge 自己的配置和状态放在 `~/Library/Application Support/Brodge`。新安装可以在 Web 扫码、私聊配对并启用机器人；已有配置用 `--config` 显式沿用。`--no-browser` 可只启动后台。Web 默认地址 `http://127.0.0.1:18789`，自动启动使用一次性登录码，管理 API 仍需认证。

当前仅 macOS，源码见 https://github.com/Donmagelas/Brodge 。源码构建需要 Go 1.25、Node.js 和 Xcode Command Line Tools（用于应用打开／重开的原生事件支持）。配置示例见 [config.example.toml](config.example.toml)。实施状态、测试证据和真实联调缺口见 [implementation.md](docs/changes/im-conversations-media/implementation.md)。
