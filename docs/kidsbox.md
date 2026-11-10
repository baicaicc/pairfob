# kidsbox 分支（baicaicc/pairfob）

本 fork 跟随上游 `arronKler/pairfob` 的 main，只在其上保留个人系统需要的少量改动。手机用自建中继 `https://pair.kidsbox.cc` 上的 PWA；上游原生 Android / iOS 壳写死 `pairfob.com`，这里不用。

## 相对上游的改动

| 改动 | 位置 | 开关 |
| --- | --- | --- |
| 所有在跑的 Herdr 会话合成一个视图：一起列出，按 `<会话>::<id>` 路由，每个会话的窗格都能推送通知；默认会话的 id 不变 | `internal/runtime/herdr_merged*.go`、`cmd/pairfob/merge_sessions.go`；PWA 在分组标题和卡片信息行显示会话名（`pwa/src/lib/dashboard.ts`、`ranking.ts`） | `PAIRFOB_MERGE_SESSIONS=1`。开启后上游的会话切换器隐藏（不再声明 `list_sessions`），带会话名的 RPC 返回 `unsupported` |
| 中继 WebSocket 走 `HTTPS_PROXY` / `HTTP_PROXY` / `NO_PROXY` | `internal/wsnet/conn.go` | 环境变量 |
| `pairfob service install` 保留上面几个变量、`PAIRFOB_AUTO_UPDATE`、`PAIRFOB_ALLINONE_CURSOR_CONFIG`；带账号密码的代理地址不写入 | `cmd/pairfob/service_runtime_fork.go` | — |
| All in One 的 Cursor ACP 会话映射（历史读取、手机输入） | 见移植提交 | `PAIRFOB_ALLINONE_CURSOR_CONFIG=<绝对路径>` |
| 自建中继配置与部署 | `workers/pairfob-origin/wrangler.kidsbox.jsonc`、`scripts/deploy-kidsbox.sh` | — |
| 本机构建 | `scripts/build-kidsbox-daemon.sh` | — |

`scripts/kidsbox-fork.test.ts` 检查 kidsbox Worker 配置与上游 `wrangler.jsonc` 对齐（除域名、D1、Analytics 绑定和原生壳 origin 外一致），以及部署脚本遇到待执行的 D1 迁移会先停下。

## 守护进程

```sh
./scripts/build-kidsbox-daemon.sh          # 在目标 Mac 上运行，产物 dist/kidsbox/pairfob
```

版本号形如 `kidsbox-20261109-abc1234`，不是发布号，也不嵌入发布签名公钥，所以上游的自动更新和手机上的手动更新都不会把它换成官方版（官方版会连回 pairfob.com）。部署时 launchd plist 另设 `PAIRFOB_AUTO_UPDATE=0`。

keluoluo 的 launchd 任务是 `com.pairfob.pairfob`，二进制 `~/.local/bin/pairfob`，plist 手工维护（不跑 `pairfob service install`），环境变量至少包括：`PAIRFOB_AUTO_UPDATE=0`、`PAIRFOB_MERGE_SESSIONS=1`、`PAIRFOB_ALLINONE_CURSOR_CONFIG`、`NO_PROXY`。中继地址来自状态目录的 `relay.json`，换二进制不影响已配对设备。

换版本：备份旧二进制与 plist → 放新二进制 → `launchctl bootout gui/$(id -u)/com.pairfob.pairfob` → `launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.pairfob.pairfob.plist` → `pairfob doctor`。退回同样步骤放回备份。

## 中继（pair.kidsbox.cc）

新 daemon 会声明新的能力键，旧 PWA 遇到不认识的键会拒绝连接，所以要先部署新的 Worker/PWA，再换 daemon。

```sh
./scripts/deploy-kidsbox.sh
```

脚本先查远端 D1 是否有待执行的迁移，有就停下（退出码 3）。迁移会改共享数据库，须先导出备份并由用户确认后再用 `PAIRFOB_APPLY_MIGRATIONS=1` 重跑：

```sh
cd workers/pairfob-origin
bunx wrangler d1 export pairfob --remote --config wrangler.kidsbox.jsonc --output ~/labs/<日期>-pairfob-d1.sql
```

Worker 代码与静态资源可用 `bunx wrangler rollback --config wrangler.kidsbox.jsonc` 回到上一版本；回滚不撤销已执行的 D1 迁移。

## 同步上游

从 `upstream/main` 开新分支，把本 fork 的提交 rebase / cherry-pick 上去；跑 `./scripts/verify.sh`（全量门槛）后合入 fork 的 main。
