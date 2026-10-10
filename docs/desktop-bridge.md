# Brodge 本机运行说明

基于 cc-connect `dfad19415a38b00b2c5c288610784d1a7eef337f` 改造成专用的 `codex-desktop` / 飞书桥接器。cc-connect 是当前试行载体，可根据真实接入结果调整。源码同步到用户指定的 Brodge 仓库。上游固定树的 README 声称 MIT，但未含 LICENSE/COPYING；分发前仍需核实，不自行联系维护者。

用户流程：手动在 Desktop 创建会话、复制链接，在飞书私聊发送 `/bind codex://threads/ID`；群聊先真实 @ 机器人。之后正常发消息、收回复。`/list` 仅列当前入口已关联的会话，`/switch ID` 在这些会话之间换绑定，`/current` 查看当前。没有额外任务编号。普通回复登记过的机器人结果可回原会话；话题根消息不能定位嵌套回复，话题中的新文字使用当前绑定。

已确认需求见 [方案文档](changes/im-conversations-media/solution.md)，逐项实现与真实验收证据见 [实施记录](changes/im-conversations-media/implementation.md)。创建者约束、入口会话列表、媒体收发与常用原生问答已实现；自动新建／`/new` 仍因 Desktop 创建入口缺失而未完成。

## 本次精简范围

Web 移除旧导航、概览统计、项目/平台创建向导、服务商/技能/定时任务/系统设置、旧聊天和 WebSocket bridge。
后端只编译 `agent/codexdesktop` 和 `platform/feishu`；删除其他 agent/IM、provider/SkillRegistry、cron/timer/heartbeat、CLI sessions、relay/hooks/runas、语音/TTS、自动更新和 daemon 安装。保留一套 Engine / config / 本机管理，不增加平行框架。

`core` 仅负责协议边界、durable 入口连接、绑定/扫码 HTTP 路由和静态 Web；不导入具体 agent/platform。未来 IM 可实现同一来源校验、持久入口和固定目的地回复接口，这次没有微信代码。配置只接受一个 Desktop 连接和最多一个飞书连接，拒绝旧全局服务商、调度、TTS 等字段。既有本机配置的旧 card/reaction 选项仍兼容读取；当前回复固定采用表情确认、终止后最终答案及有序媒体投递。

消息 worker 轮询、出站重试和机器人身份恢复属于消息可靠性，继续保留；它们不提供用户定时任务或定时模型调用。IPC 同步继续使用现有 Desktop 进程与已授权会话，不更改用户 Codex 配置。

## 构建和运行

本机 Go 在 `$HOME/.cache/desktop-bridge/go/bin`，版本 1.25.8，官方归档 SHA-256 已核对。没有改系统 PATH。以下 PATH 仅对该命令生效：

```sh
PATH=$HOME/.cache/desktop-bridge/go/bin:$PATH GOTOOLCHAIN=local GOPROXY=https://goproxy.cn,direct make build
./Brodge --config config.desktop.local.toml
```

前台 Ctrl-C 停止；只停止桥接，Desktop 继续运行。Mac 可用 `scripts/package-macos.sh /绝对配置路径` 打包 `build/Brodge.app`，双击启动并自动打开普通浏览器；重复双击复用后台并重新开页。Web“关闭 Brodge”停止后台，无法关闭的标签显示已退出；直接关标签时后台继续。没有安装 launchd 或登录自启动。真实配置含凭证时只在本机使用权限 0600 的文件，且不提交。配置示例：

```toml
data_dir = "/absolute/path/to/Brodge/.state"
language = "zh"
[management]
enabled = true
port = 18789
token = "${DESKTOP_BRIDGE_ADMIN_TOKEN}"

[[projects]]
name = "desktop"
[projects.agent]
type = "codex-desktop"
[projects.agent.options]
protocol = "local-v11-start-v2"
allowed_threads = ["替换为明确授权的已有Desktop会话ID"]
native_answers = false
# socket = "/absolute/path/to/desktop/ipc.sock"

[[projects.platforms]]
type = "feishu"
[projects.platforms.options]
app_id = "你的应用ID"
app_secret = "${DESKTOP_BRIDGE_FEISHU_SECRET}"
tenant_key = "你的租户key"
allow_from = "指定操作者的open_id"
```

环境变量未设置会展开为空；启动前应通过隐藏输入设置 Secret，并在本机生成非空管理 token（例如 `export DESKTOP_BRIDGE_ADMIN_TOKEN=$(openssl rand -hex 32)`，不打印）。Desktop 项目管理不接受空 token。

Web 管理在 `http://127.0.0.1:18789`，用管理 token 登录。页面仅包含飞书扫码/配对、已有会话绑定、连接诊断、消息和待投递回复状态。服务商、桥接器技能管理、用户定时任务、旧 Web 聊天和通用系统设置已从代码和 HTTP 路由移除；对应页面/API 返回 404。Codex Desktop 自己的技能、模型、权限和网关能力继续正常工作。只允许已配对的创建者使用；创建者可私聊或在任意已加入机器人的群内 @ 它，不设群白名单。各群入口独立绑定。首次绑定可直接在飞书 `/bind`。App Secret 使用终端隐藏输入设置环境变量，例如 zsh `read -s 'DESKTOP_BRIDGE_FEISHU_SECRET?App Secret: '` 后 `export DESKTOP_BRIDGE_FEISHU_SECRET`，不要输入到聊天。管理 token 也只在本机生成和使用。默认不读取 Codex auth、模型或网关文件。

## 扫码创建应用

会话页新增扫码创建区块，沿用 Bro 的官方 PersonalAgent 注册流程。二维码通过 `accounts.feishu.cn` 获取，用户在飞书官方页面确认；后端保存应用凭据，再检查 tenant token 和机器人信息。页面只返回创建状态、App ID 和创建人，不返回 device code、App Secret 或访问令牌。

每个 Desktop 项目的创建记录保存于 `data_dir/desktop/<项目>/feishu-setup.json`，权限 0600，原子替换；密钥是本机明文，与消息数据库的隐私边界相同。重复点击复用活动流程；刷新继续查看进度；重启可继续已有二维码或检查已创建的应用。取消不会删除已经返回的应用凭据，也不删除飞书中的应用。

“检查通过”仅表示凭据和机器人 API 可用，尚未开启消息转发。点击“配对测试私聊”，由创建人在新机器人的私聊中发送十分钟有效的配对码；只接受匹配应用、租户、创建人和完整配对码的私聊事件，记录租户和聊天标识。配对码不会交给 Codex，不产生模型回合或外发消息。取消、超时和重启关闭配对监听，保留应用凭据；配对成功保留私聊来源，刷新或重新检查应用不重复配对。没有创建人身份时不会开放配对。

临时配对使用同一可执行文件的短期子进程运行现有官方 WebSocket SDK，因为 SDK 的 Start 不提供可可靠取消的关闭接口；凭据仅通过 stdin 私有管道传入，日志不输出连接数据，父进程等待子进程退出后才认为完成。之后仍需明确指定可丢弃的 Desktop 会话并启动正式转发。原有通用扫码保存接口已删除；创建流程不覆盖 Desktop 模型/网关或已有飞书连接。创建区块可随后续 Web 改版移动。

正式程序启动时会登记已认证的配对来源；首次登记且配置仅授权一个 Desktop 会话时自动绑定该会话。零个或多个授权会话时保持未绑定，允许在 Web 提前选择或由首条正文发起创建。不会回复配对消息或把配对码交给模型。重复启动保持绑定修订不变，来源冲突拒绝启动。

## 数据和恢复

SQLite：`.state/desktop/desktop/bridge.sqlite`，WAL + synchronous FULL。应用/租户/message ID 入站去重，冲突来源/内容拒绝。每条消息固定会话与源聊天，同一个会话串行。服务文件锁限制同状态目录单实例，claim 每次使用随机 owner 代次。

已登记未提交消息可继续；提交 intent 后崩溃/断线则标 uncertain，只通过确切 clientUserMessageId/turn ID 的 Desktop 快照核对，不补发。找不到证据时保留 uncertain、阻塞该会话后续队列；查看 Web 状态和 Desktop 后人工处理，不删除数据库来“恢复”。快速连续发送 /bind 或 /switch 后的正文依入站顺序使用该命令的结果；Web 之后的切换不改投已接受的正文。运行状态使用固定输出记录去重。

原生答案的 ACK 不代表已回答。常用异步弹窗核对原 turn 内确切 questionItemId、题目和 accepted 回答；同步请求核对原请求消失及对应答案证据。回合结束会关闭未完成问题。

出站固定 UUID/内容/目的地，按 Unicode 与实际 JSON 编码预算分段；同一结果前段未送达时后段等待。复杂／超长格式额外附逐字 Markdown 文件，图片／附件经官方 API 上传，快照 SHA、上传 key 与发送 UUID 持久保留。官方 SDK 可能在无效 token 或拨号失败时重试，同一个调用保持原 UUID；桥接器负责总时限。未知发送仅在首次尝试 3500 秒内用原 UUID 核对重试，过期进入 reconcile；权限/成员/不可见目标等错误进入 blocked，不换群、不重定向。Web 页展示执行和出站状态。不要运行使用不同状态目录但接收同一应用范围的第二个桥接服务；飞书长连接随机分配事件，独立存储无法共享去重。SDK 重连不能保证完全离线期间消息补发。

## 当前边界

- IPC 为未公开稳定承诺的版本限定协议：initialize 0、owner 1、history 1、start 2、stream 11。未知版本停止，无降级/CLI/app-server 回退。
- 基础会话绑定、文字提交、结果、普通追问、持久队列和错误/恢复有本机离线测试；不等于真实 Desktop/飞书闭环。
- 新适配器在本机现有 Desktop socket 的初始化、指定测试会话的历史读取，以及三轮真实私聊问答已通过。2026-10-09 的真实测试返回“已记住青松47”→“青松47”→“青松48”；三个回合均来自用户指定的同一 Desktop 会话，回复经飞书官方消息读取 API 回查一致，用户逐轮确认收到。正式桥接程序重启后仍沿用同一绑定。此证据仅覆盖该私聊与该测试会话。
- completed 与错误分别读取，错误/过滤/流中断不报任务成功。正常结束只报告输出，不证明工作成果正确。
- 常用异步弹窗的选项和自填两条路径已在原 Desktop turn 内真实通过，本机 Brodge 配置已开启 `native_answers=true`。终止时先核对答案证据，禁止继续提交。同步 requestUserInput 和其他 picker 尚无完整真实验收；未知表单提示 Desktop 处理。远程权限审批不在本阶段。
- 上游 `AgentSession` / CLI 生命周期和重复的 Web 聊天镜像已删除；真实发送只走持久入口，历史仅在内部通过已授权 IPC 快照用于结果归属及恢复。
- 只验证 macOS；Unix 文件锁与 socket 实现未提供 Windows 适配。
- `/new` 暂缓；自动无项目创建的原生适配和持久接续已实现并通过模拟测试，真实界面尚待验收；未加载的会话会尝试限频深链接打开并重新连接；真实冷启动全链路尚未验收。图片／附件双向已真实验收，群媒体、复杂格式手机显示尚未验收。取消、模型/权限/工作目录管理、全部会话枚举、多机及共享用户协作不在当前范围。
- 同会话桥接队列串行，无法证明与用户在 Desktop 同时提交的原子互斥；观察到 busy 就等待，只转发本桥接器的回合。

## 可复现验证

从仓库根目录执行，Go 环境仅在命令中生效。`TMPDIR=/private/tmp` 避免上游 macOS 测试的 `/var` 与 `/private/var` 路径差异。

```sh
TMPDIR=/private/tmp GOTOOLCHAIN=local GOPROXY=https://goproxy.cn,direct $HOME/.cache/desktop-bridge/go/bin/go test ./agent/codexdesktop ./core ./platform/feishu ./cmd/brodge
TMPDIR=/private/tmp GOTOOLCHAIN=local GOPROXY=https://goproxy.cn,direct $HOME/.cache/desktop-bridge/go/bin/go test -race ./agent/codexdesktop ./core ./platform/feishu -run 'Desktop|NativeAnswer|RepeatedQuestion|RapidBind|LongReply|RecoveredOrigin'
TMPDIR=/private/tmp GOTOOLCHAIN=local GOPROXY=https://goproxy.cn,direct $HOME/.cache/desktop-bridge/go/bin/go test ./agent/codexdesktop -run TestCUJ
GOTOOLCHAIN=local GOPROXY=https://goproxy.cn,direct $HOME/.cache/desktop-bridge/go/bin/go vet ./agent/codexdesktop ./core ./platform/feishu ./cmd/brodge
TMPDIR=/private/tmp GOTOOLCHAIN=local GOPROXY=https://goproxy.cn,direct $HOME/.cache/desktop-bridge/go/bin/go test ./...
./Brodge desktop-check
```

`desktop-check` 默认只 initialize 并关闭连接，不访问会话或模型。只有明确指定 `--thread ID` 才加载该会话；真实联调前使用另行指定的可丢弃会话，不能取当前开发会话。该检查不能证明模型/网关路线或对话闭环。

本地用例使用真实 Unix socket、实际 SQLite 事务、进程退出后的恢复，以及飞书官方 Go SDK v3.5.3 对本地 HTTP 服务的请求和事件分发；Desktop 和飞书服务边界仍为模拟。精简版保留实际 Desktop socket/SQLite/恢复/飞书 SDK 测试，删除已移除产品的测试；全仓库测试、race、vet、Web 和 binary 构建通过。回归测试验证服务商/技能/定时任务/系统设置/旧会话入口的 GET、POST、PATCH、DELETE 均返回 404，现有绑定和配对入口仍需管理 token。

本次真实测试的本机运行配置为根目录 `config.desktop.local.toml`，权限 0600，不提交；只放行一个测试私聊和一个明确指定的 Desktop 会话。目前可使用 `build/Brodge.app` 双击运行，包内配置路径指向同一私有配置，Web 仍在 18789 端口，没有安装自启动。创建记录在 `.state/web-preview/desktop/desktop/feishu-setup.json`，消息数据库在 `.state/web-preview/state/bridge.sqlite`。扫码创建与配对已进入产品代码；Web 已支持启用配对后的飞书连接；新的功能和验收结论以 [implementation.md](changes/im-conversations-media/implementation.md) 为准。Desktop 路径的 SDK 日志只保留固定故障信号，不输出整条消息或连接凭据。

## 真实联调最小条件

明确指定可丢弃 Desktop 会话链接（不能是当前开发会话），授权固定的文字往返/两轮追问/绑定切换步骤；原生回答另列固定步骤，不能扩大到任意工具审批。

扫码与私聊配对可取得飞书应用、租户 key、操作者 open_id 和私聊 chat_id，App Secret 留在本机。需要机器人能力、应用发布/可用范围、长连接事件 `im.message.receive_v1`，权限 `im:message.p2p_msg:readonly`、`im:message.group_at_msg:readonly`、`im:message:send_as_bot`。本次私聊条件已满足；用户已准备 `test` 群并添加机器人，真实群聊闭环尚待执行，原生问题远程回答、工具审批和 `/new` 不包含在三轮文字测试的通过结论中。

2026-10-09 精简后换版验证：原 `config.desktop.local.toml`、管理 token 和 canonical 飞书配对记录逐字节未变；数据库保留 1 个 revision=1 绑定、3 个 finished 回合、9 条 sent 回复，无待发送项。新二进制 18 MB（旧全量 Web 预览程序约 73 MB）；Web JS 370 KB。实际打开原 Web 地址后显示长连接已连接及原三轮消息状态；已移除页面和全局/项目 API 在运行服务上返回 404。

精简版真实回归也已完成：用户在原机器人私聊发送“把刚才的新暗号原样回复，只回复暗号，不使用工具”，收到“青松48”。新程序的 SQLite 回合归属仍是原授权 Desktop 会话；出站结果为 sent，经飞书官方消息 GET API 回查内容与原私聊一致。截至该次精简回归累计 4 个 finished 回合、12 条 sent 回复，原绑定 revision 仍为 1，无待发送/失败项。
