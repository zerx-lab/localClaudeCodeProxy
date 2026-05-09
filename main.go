// localClaudeCodeProxy 是基于 Wails3 的桌面应用，把本地 Claude Code 订阅暴露成本地 HTTP API。
//
// 关键设计：
//   - **关闭主窗口不退出进程**：Mac.ApplicationShouldTerminateAfterLastWindowClosed = false，
//     Windows 端通过 WindowsWindow.HiddenOnTaskbar + WindowClosing 钩子实现"最小化到托盘"。
//   - **系统托盘常驻**：左键打开主窗口，右键菜单 Show / Quit。
//   - **HTTP 代理由前端按钮控制**：见 ProxyService.Start/Stop。
package main

import (
	"embed"
	"log"
	"runtime"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"

	"localclaudecodeproxy/internal/settings"
)

//go:embed all:frontend/dist
var assets embed.FS

func init() {
	// 注册自定义事件类型，让 wails 的 binding 生成器在前端拿到强类型 API。
	application.RegisterEvent[LogEntry]("proxy:log")
}

func main() {
	// 应用本身：关掉最后一个窗口不退出进程，只能从托盘 Quit。
	app := application.New(application.Options{
		Name:        "localClaudeCodeProxy",
		Description: "Local proxy for Claude Code subscription",
		Mac: application.MacOptions{
			ApplicationShouldTerminateAfterLastWindowClosed: false,
		},
		Assets: application.AssetOptions{
			Handler: application.AssetFileServerFS(assets),
		},
	})

	// 加载用户偏好；首次启动文件不存在不算错误，settings.Default() 兜底。
	store, loadErr := settings.NewStore()
	if loadErr != nil {
		log.Printf("settings load warning: %v", loadErr)
	}

	// 在 app 创建之后注入 service：service 需要 app 引用来 EmitEvent。
	proxySvc := NewProxyService(app, store)
	app.RegisterService(application.NewService(proxySvc))

	// 主窗口：关掉时只是隐藏到托盘，不真的销毁。
	//
	// Titlebar 策略（前后端配合，前端绘制 36px 高的自定义 titlebar）：
	//   - Windows：Frameless=true，丢掉系统 titlebar，前端自绘最小化/最大化/关闭按钮，
	//     拖动区由 CSS `--wails-draggable: drag` 标记。
	//   - macOS：保留 MacTitleBarHiddenInset，让原生红绿灯按钮浮在自定义 titlebar 上，
	//     前端 titlebar 左侧预留 80px 空间避让红绿灯。
	winOpts := application.WebviewWindowOptions{
		Title:            "localClaudeCodeProxy",
		Width:            760,
		Height:           680,
		MinWidth:         640,
		MinHeight:        520,
		BackgroundColour: application.NewRGB(15, 17, 23),
		URL:              "/",
		Windows: application.WindowsWindow{
			HiddenOnTaskbar: false,
		},
		Mac: application.MacWindow{
			InvisibleTitleBarHeight: 38,
			Backdrop:                application.MacBackdropTranslucent,
			TitleBar:                application.MacTitleBarHiddenInset,
		},
	}
	if runtime.GOOS == "windows" {
		// Windows 端走 Frameless，否则会出现"系统 titlebar + 自定义 titlebar"双重叠加。
		winOpts.Frameless = true
	}
	window := app.Window.NewWithOptions(winOpts)

	// 关窗钩子：行为由用户偏好 HideOnClose 决定。
	//   - true（默认）: 隐藏到托盘，应用继续在后台运行。
	//   - false:        放行默认关闭，配合 ApplicationShouldTerminateAfterLastWindowClosed=false
	//                   仍不会真正退出进程；用户期望"完全退出"应通过托盘菜单 Quit。
	window.RegisterHook(events.Common.WindowClosing, func(e *application.WindowEvent) {
		if proxySvc.HideOnClose() {
			window.Hide()
			e.Cancel()
		}
		// HideOnClose=false：不 Cancel，让窗口正常关闭。进程仍由托盘维持，因为
		// MacOptions.ApplicationShouldTerminateAfterLastWindowClosed 是 false。
	})

	// 应用启动后：如果用户开了"自动启动代理"，立刻拉起一次代理。
	app.Event.OnApplicationEvent(events.Common.ApplicationStarted, func(_ *application.ApplicationEvent) {
		if !proxySvc.AutoStartProxy() {
			return
		}
		host, port := proxySvc.LastEndpoint()
		if _, err := proxySvc.Start(host, port); err != nil {
			log.Printf("auto-start proxy failed: %v", err)
		}
	})

	// 系统托盘：左键 = 显示窗口；菜单提供 Show/Quit。
	tray := app.SystemTray.New()
	tray.SetLabel("localClaudeCodeProxy")

	menu := app.NewMenu()
	menu.Add("显示主窗口").OnClick(func(*application.Context) {
		window.Show()
	})
	menu.AddSeparator()
	menu.Add("退出").OnClick(func(*application.Context) {
		// 退出前优雅停止代理服务
		proxySvc.shutdown()
		app.Quit()
	})
	tray.SetMenu(menu)
	tray.OnClick(func() {
		window.Show()
	})

	if err := app.Run(); err != nil {
		log.Fatal(err)
	}
}
