# AGENTS.md — localClaudeCodeProxy

## 1. 项目目标 (业务上下文)

把本地 Claude Code 订阅（OAuth 凭证）暴露成本地 HTTP API，让其他工具（OpenCode、IDE 插件、CLI 等）以 Anthropic API 的形式调用，而不消耗 API key 配额。

成品形态：

- **Wails3 桌面应用**：Go 后端 + React 前端 + 系统托盘常驻
- **UI 开关**：前端按钮控制 Go 后端的 HTTP 代理服务启停
- **后台运行**：关闭主窗口不退出进程，最小化到任务栏托盘
- **代理逻辑**：参考 `C:\Users\zero\Desktop\code\github\opencode-claude-auth` 的 TypeScript 实现，用 Go 重写为 HTTP server

## 2. 当前状态 (重要)

仓库目前几乎是 **wails3 init 生成的全新模板**，业务代码尚未开始：

- `main.go` — 模板，注册了示例 `GreetService`，每秒发 `time` 事件
- `greetservice.go` — 7 行的 `Greet(name)` 示例
- `frontend/src/App.tsx` — 模板 UI，演示绑定调用与事件监听
- 模块名仍然是 **`changeme`**（go.mod + 前端 import）
- `README.md` 是 wails3 模板文案，**不是项目说明**
- `.git` **不存在**（"Is directory a git repo: no"）。需要 git 操作前先 `git init`

未来 agent 看到 `GreetService`、`changeme` 不要以为是业务代码，是模板残留。

## 3. 工具链 (已验证可用)

| 工具    | 版本                | 用途                  |
| ------- | ------------------- | --------------------- |
| Go      | 1.26.1 (go.mod 写 1.25) | 后端                  |
| Node    | v24.14.1            | 前端构建              |
| npm     | 11.11.0             | 前端依赖（**不是 pnpm**） |
| task    | 3.49.1              | 任务编排（`go-task`） |
| wails3  | v3.0.0-alpha.78     | 框架 CLI              |

**Wails3 处于 ALPHA 阶段，API 不稳。** v3.wails.io 上很多文档页 404（systray、services 等都 404 过），权威参考是模块缓存里的 examples：

```
C:\Users\zero\go\pkg\mod\github.com\wailsapp\wails\v3@v3.0.0-alpha.78\examples\
```

特别有用：`hide-window/`、`systray-menu/`、`systray-basic/`、`systray-clock/`。**写 wails 代码遇到不确定的 API 先去 examples 找，不要凭训练数据猜。**

## 4. 命令 (必须走 task，不要直接 wails3)

`Taskfile.yml` 在仓库根目录，包装了 wails3 + 跨平台逻辑。直接 `wails3 dev` 会丢失 `-config` 参数和 vite 端口配置。

```pwsh
# 开发（热重载）。会启动 vite (端口 9245) + 重建 Go + 启动应用
task dev

# 生产构建（输出 bin/localClaudeCodeProxy.exe）
task build

# 打包（NSIS 安装包等）
task package

# 仅运行已构建的二进制
task run

# 列出全部子任务（含 windows: / common: 命名空间）
task --list
```

`task dev` 内部依次执行（见 `build/config.yml` 的 `dev_mode.executes`）：

1. `wails3 build DEV=true`（blocking）
2. `wails3 task common:dev:frontend`（background）
3. `wails3 task run`（primary）

监听的源文件扩展名：`*.go`、`*.js`、`*.ts`（**`frontend/` 目录被整体 ignore**，前端热重载靠 vite 自己）。

## 5. 关键架构约定

### 5.1 Go ↔ JS 绑定自动生成

`frontend/vite.config.ts` 用 `@wailsio/runtime/plugins/vite` 插件，把 Go 端 service 方法自动生成到 `frontend/bindings/<module-name>/`。当前模块名是 `changeme`，所以前端 import 写：

```ts
import { GreetService } from "../bindings/changeme";
```

**改 `go.mod` 模块名 = 改前端 import 路径 + 重新生成 bindings**。如果要重命名，最好同时改：

- `go.mod` 第 1 行
- `frontend/src/App.tsx` 的 import
- `frontend/tsconfig.json` 的 `include`（已包含 `bindings`）
- 然后跑 `task dev` 让插件重新生成

### 5.2 Service 注册模式

```go
app := application.New(application.Options{
    Services: []application.Service{
        application.NewService(&GreetService{}),
        // 新增 service 在这里追加
    },
    ...
})
```

Service 是普通 Go struct，所有**导出方法**都会暴露给前端。可选实现：
- `ServiceName() string` — 自定义名字
- `OnStartup(ctx context.Context, options application.ServiceOptions) error` — 启动钩子

如果想把 service 挂成 HTTP handler 而不是 RPC，给 `NewServiceWithOptions` 传 `Route: "/api/proxy"` 并让 struct 实现 `http.Handler`。

### 5.3 系统托盘 + 关闭即隐藏 (本项目核心范式)

参考 `examples/hide-window/main.go`，关键代码段：

```go
window := app.Window.NewWithOptions(application.WebviewWindowOptions{
    Windows: application.WindowsWindow{
        HiddenOnTaskbar: true,  // Windows 任务栏隐藏，仅在托盘
    },
})

window.RegisterHook(events.Common.WindowClosing, func(e *application.WindowEvent) {
    window.Hide()
    e.Cancel()  // 阻止默认关闭
})

systemTray := app.SystemTray.New()
menu := app.NewMenu()
menu.Add("Show").OnClick(func(ctx *application.Context) { window.Show() })
menu.Add("Quit").OnClick(func(ctx *application.Context) { app.Quit() })
systemTray.SetMenu(menu)
systemTray.OnClick(func() { window.Show() })
```

注意：`main.go` 当前的 `Mac.ApplicationShouldTerminateAfterLastWindowClosed: true` **必须改为 `false`**，否则关窗即退出，没法常驻。

## 6. 参考实现 (opencode-claude-auth 移植要点)

完整源在 `C:\Users\zero\Desktop\code\github\opencode-claude-auth\src\`。是 TypeScript 写的 OpenCode 插件，要用 Go 改写为本地 HTTP 代理。**不要照搬命名（plugin/loader/fetch hook 是 OpenCode 概念，本项目用不上）**，照搬的是协议细节。

### 6.1 凭证来源 (Windows)

macOS keychain 在 Windows 上**不可用**，跳过 `keychain.ts` 全部逻辑。Windows 唯一来源：

```
%USERPROFILE%\.claude\.credentials.json
```

JSON 结构（已验证存在）：

```json
{
  "claudeAiOauth": {
    "accessToken": "sk-ant-oat01-...",
    "refreshToken": "sk-ant-ort01-...",
    "expiresAt": 1234567890000,        // 毫秒时间戳
    "scopes": ["user:inference", ...],
    "subscriptionType": "max",
    "rateLimitTier": "..."
  }
}
```

参考解析逻辑：`src/keychain.ts` 的 `parseCredentials()`。

### 6.2 OAuth 刷新

```
端点:    https://claude.ai/v1/oauth/token
方法:    POST application/x-www-form-urlencoded
client_id: 9d1c250a-e61b-44d9-88ed-5944d1962f5e
body:    grant_type=refresh_token&client_id=<id>&refresh_token=<token>
```

返回 `{access_token, refresh_token, expires_in}`，`expires_in` 缺省按 36000 秒（10h）。
源：`src/credentials.ts` 的 `refreshViaOAuth()`、`parseOAuthResponse()`。
内存缓存 TTL：**30 秒**。后台同步 interval：**5 分钟**。

### 6.3 转发到 Anthropic 的强制 headers

调用 `https://api.anthropic.com/v1/messages` 时，**必须**注入这些头（否则会被识别为非 Claude Code 客户端而拒绝/限流）：

```
authorization: Bearer <accessToken>
anthropic-version: 2023-06-01
anthropic-beta: claude-code-20250219,oauth-2025-04-20,interleaved-thinking-2025-05-14,prompt-caching-scope-2026-01-05,context-management-2025-06-27,advisor-tool-2026-03-01
anthropic-dangerous-direct-browser-access: true
x-app: cli
user-agent: claude-cli/2.1.112 (external, sdk-cli)
x-client-request-id: <uuid per request>
X-Claude-Code-Session-Id: <stable per-process uuid>
x-stainless-arch / -lang / -os / -package-version / -retry-count / -runtime / -runtime-version / -timeout
```

并且**显式删除** 上游传来的 `x-api-key`。源：`src/index.ts` 的 `buildRequestHeaders()`、`getStainlessHeaders()`。

### 6.4 请求 body 改写 (容易踩坑)

照抄 `src/transforms.ts` 的两条规则：

1. **system message 前缀**：`system[]` 的第一条文本块**必须**以 `"You are Claude Code, Anthropic's official CLI for Claude."` 开头，否则被识别为非 CC 客户端。
2. **工具名前缀**：转发到上游前，所有 tool 名字加 `mcp_` 前缀且**首字母大写**（`bash` → `mcp_Bash`）；流式响应里再剥掉前缀返回给客户端。Claude Code 用 PascalCase tool 名，小写名会触发拒绝。

### 6.5 重试与限流

429/529 重试至多 3 次，遵循 `retry-after`。**`retry-after > 30 秒` 直接放弃**（视为配额耗尽，不要傻等几小时）。源：`src/index.ts` 的 `fetchWithRetry()`，可由 `OPENCODE_CLAUDE_AUTH_MAX_RETRY_MS` 环境变量覆盖。

### 6.6 长上下文 betas

如果上游报"long context required"类错误，逐个剔除 `interleaved-thinking-2025-05-14`、`context-1m-2025-08-07` 重试。源：`src/betas.ts`。1M 上下文仅 `opus`/`sonnet` 4-x 系列支持。

### 6.7 不需要移植的部分

- `keychain.ts` macOS 多账户管理（Windows 用不到）
- `signing.ts` billing header 的 `cch`/`version suffix`（属于 OpenCode 计费追踪，本项目纯代理可不做；做了更逼真）
- `plugin-config.ts`（OpenCode 配置注入机制不适用）
- `auth.json` 双路径同步（OpenCode 专用，本项目 HTTP 代理不需要）

## 7. 测试与验证

- **没有测试基础设施**。要写 Go 单测就直接 `go test ./...`，没有 lint/format 配置文件。
- 前端 lint 配置不存在，仅 `tsc` 由 `vite build` 触发。
- 验证代理是否工作：起服务后用 `curl` 或 `Invoke-WebRequest` 直打本地端口，对比官方 `claude` CLI 的请求头（`%USERPROFILE%\.claude\` 下的日志可作旁证）。
- 改 wails 代码后测试 GUI 行为**必须实际启动 `task dev` 看托盘和窗口**，单跑 `go build` 不能验证 webview 行为。

## 8. 仓库布局速查

```
.
├── main.go              # wails 应用入口，注册 services / 创建窗口
├── greetservice.go      # 模板示例 service（待删/替换）
├── go.mod               # module changeme （待重命名）
├── Taskfile.yml         # 顶层任务，平台特定的在 build/<os>/Taskfile.yml
├── build/
│   ├── config.yml       # 应用元信息 + dev_mode 配置
│   ├── windows/         # NSIS、MSIX、icon、syso 生成
│   └── (darwin/linux/android/ios/docker)/
└── frontend/
    ├── package.json     # react 18 + vite 5 + @wailsio/runtime
    ├── vite.config.ts   # 装载 wails vite 插件，bindings 输出到 ./bindings
    ├── index.html
    ├── public/          # style.css, 图标, Inter 字体
    ├── src/             # App.tsx / main.tsx / vite-env.d.ts
    └── bindings/        # 自动生成（gitignored），Go service → TS
```

`.gitignore` 已忽略：`.task`、`bin`、`frontend/dist`、`frontend/node_modules`、appimage build、edge webview installer。

## 9. 易踩的坑汇总

1. 直接 `wails3 dev` 而不是 `task dev` → vite 端口/配置错乱
2. 改 `go.mod` 模块名忘记同步前端 import（`bindings/changeme`）→ 编译失败
3. 在 `main.go` 留着 `ApplicationShouldTerminateAfterLastWindowClosed: true` → 关窗即退出，托盘失效
4. 在 v3.wails.io 找文档 → 多个页 404，**先看 `~/go/pkg/mod/.../wails/v3@.../examples/`**
5. Windows 上去找 macOS keychain → 不存在，只读 `~/.claude/.credentials.json`
6. 转发请求漏了 `anthropic-beta`、`X-Claude-Code-Session-Id`、`user-agent` 任意一项 → 被识别为非 CC 客户端
7. 转发请求保留了上游传来的 `x-api-key` → 与 OAuth Bearer 冲突
8. 改了 system message 但没保留 "You are Claude Code..." 前缀 → 拒绝
9. 工具名转发时没加 `mcp_` PascalCase 前缀 → 拒绝
10. 仓库**没有 git**，未来 commit 前先 `git init` + `.gitignore` 已存在不要覆盖
