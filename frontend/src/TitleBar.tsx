// TitleBar 是跨平台的自定义 titlebar 组件，配合 main.go 里的窗口选项工作：
//
//   - Windows: main.go 设置了 Frameless=true，本组件提供完整的拖动条 + 最小化/最大化/关闭按钮。
//   - macOS:   main.go 用 MacTitleBarHiddenInset，原生红绿灯按钮浮在内容上，
//             本组件左侧预留 80px 让红绿灯不被遮挡；不绘制自定义控制按钮。
//
// 拖动区由 CSS 自定义属性 `--wails-draggable: drag` 标记；按钮等交互区用 `no-drag` 反向声明。
import { useEffect, useState, type CSSProperties } from "react";
import { Window, System } from "@wailsio/runtime";

type Platform = "windows" | "darwin" | "linux" | "unknown";

function detectPlatform(): Platform {
  // System.IsWindows / IsMac 是同步函数，但只在 wails runtime 注入后可用。
  // 在浏览器预览（vite dev 直访）下 fallback 为 unknown，组件会按 windows 风格渲染。
  try {
    if (System.IsWindows()) return "windows";
    if (System.IsMac()) return "darwin";
    if (System.IsLinux()) return "linux";
  } catch {
    // wails runtime 还没就绪
  }
  return "unknown";
}

interface TitleBarProps {
  title: string;
  /** 右侧附加内容，比如状态徽章。会显示在控制按钮左边。 */
  extra?: React.ReactNode;
}

export default function TitleBar({ title, extra }: TitleBarProps) {
  const [platform, setPlatform] = useState<Platform>(() => detectPlatform());
  const [maximised, setMaximised] = useState(false);

  // wails runtime 注入有可能晚于 React 第一次渲染，挂载后再探一次。
  useEffect(() => {
    setPlatform(detectPlatform());
  }, []);

  const isMac = platform === "darwin";
  const isWin = platform === "windows" || platform === "unknown";

  // 双击 titlebar 切换最大化（Windows 习惯做法）。macOS 系统会自带处理，跳过。
  const onDoubleClick = () => {
    if (!isWin) return;
    Window.ToggleMaximise()
      .then(() => Window.IsMaximised())
      .then((m) => setMaximised(Boolean(m)))
      .catch(() => {});
  };

  const onMin = () => {
    Window.Minimise().catch(() => {});
  };
  const onMax = () => {
    Window.ToggleMaximise()
      .then(() => Window.IsMaximised())
      .then((m) => setMaximised(Boolean(m)))
      .catch(() => {});
  };
  const onClose = () => {
    // Windows 上 main.go 的 WindowClosing 钩子会拦截 Close 把窗口隐藏到托盘，符合预期。
    Window.Close().catch(() => {});
  };

  // 关键：拖动区通过 CSS 变量声明，不需要监听 mousedown。
  const draggableStyle: CSSProperties = {
    // @ts-expect-error 自定义 CSS 属性
    "--wails-draggable": "drag",
  };
  const noDragStyle: CSSProperties = {
    // @ts-expect-error 自定义 CSS 属性
    "--wails-draggable": "no-drag",
  };

  return (
    <div
      className={`titlebar ${isMac ? "titlebar--mac" : "titlebar--win"}`}
      style={draggableStyle}
      onDoubleClick={onDoubleClick}
    >
      <div className="titlebar__left">
        <span className="titlebar__logo" aria-hidden>
          {/* 简单的盾牌+对勾 SVG，强调"代理 + 安全"语义 */}
          <svg
            width="16"
            height="16"
            viewBox="0 0 24 24"
            fill="none"
            xmlns="http://www.w3.org/2000/svg"
          >
            <path
              d="M12 2.5 4 5v6.5c0 4.6 3.2 8.6 8 10 4.8-1.4 8-5.4 8-10V5l-8-2.5Z"
              stroke="currentColor"
              strokeWidth="1.6"
              strokeLinejoin="round"
            />
            <path
              d="m8.6 12.4 2.5 2.5 4.3-4.6"
              stroke="currentColor"
              strokeWidth="1.6"
              strokeLinecap="round"
              strokeLinejoin="round"
            />
          </svg>
        </span>
        <span className="titlebar__title">{title}</span>
      </div>

      <div className="titlebar__right" style={noDragStyle}>
        {extra && <div className="titlebar__extra">{extra}</div>}
        {isWin && (
          <div className="titlebar__controls" style={noDragStyle}>
            <button
              type="button"
              className="titlebar__btn"
              aria-label="最小化"
              onClick={onMin}
            >
              <svg width="12" height="12" viewBox="0 0 12 12" aria-hidden>
                <rect x="2" y="5.5" width="8" height="1" fill="currentColor" />
              </svg>
            </button>
            <button
              type="button"
              className="titlebar__btn"
              aria-label={maximised ? "还原" : "最大化"}
              onClick={onMax}
            >
              {maximised ? (
                <svg width="12" height="12" viewBox="0 0 12 12" aria-hidden>
                  <rect
                    x="3.5"
                    y="2.5"
                    width="6"
                    height="6"
                    fill="none"
                    stroke="currentColor"
                  />
                  <rect
                    x="2.5"
                    y="3.5"
                    width="6"
                    height="6"
                    fill="none"
                    stroke="currentColor"
                  />
                </svg>
              ) : (
                <svg width="12" height="12" viewBox="0 0 12 12" aria-hidden>
                  <rect
                    x="2.5"
                    y="2.5"
                    width="7"
                    height="7"
                    fill="none"
                    stroke="currentColor"
                  />
                </svg>
              )}
            </button>
            <button
              type="button"
              className="titlebar__btn titlebar__btn--close"
              aria-label="关闭"
              onClick={onClose}
            >
              <svg width="12" height="12" viewBox="0 0 12 12" aria-hidden>
                <path
                  d="M2.5 2.5l7 7M9.5 2.5l-7 7"
                  stroke="currentColor"
                  strokeWidth="1.1"
                  strokeLinecap="round"
                />
              </svg>
            </button>
          </div>
        )}
      </div>
    </div>
  );
}
