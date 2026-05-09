// App.tsx 是 localClaudeCodeProxy 的主界面：账户状态 / 代理启停 / 实时日志。
//
// 数据来源：
//   - bindings/localclaudecodeproxy 由 Wails vite 插件根据 Go service 自动生成。
//   - "proxy:log" 事件由后端 ProxyService.emitLog 推送，订阅后实时刷新日志面板。
//   - GetLogs() 接口返回后端环形缓冲区，刷新按钮或初始化时调用。
import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Events } from "@wailsio/runtime";
import { ProxyService } from "../bindings/localclaudecodeproxy";
import type {
  LogEntry,
  ProxyStatus,
  SettingsView,
} from "../bindings/localclaudecodeproxy/models";
import { SettingsInput } from "../bindings/localclaudecodeproxy/models";
import type { AccountInfo } from "../bindings/localclaudecodeproxy/internal/ccproxy/models";
import TitleBar from "./TitleBar";

const MAX_LOG_LINES = 200;

// LogItem 是展示层用的日志条目，在 LogEntry 基础上多一个稳定 uid。
// uid 在客户端内部生成，避免刷新/带入新条目时 index 错位导致展开状态混乱。
type LogItem = LogEntry & { _uid: number };

const STATUS_LABEL: Record<"running" | "stopped", string> = {
  running: "运行中",
  stopped: "已停止",
};

function formatExpire(iso: string): string {
  if (!iso) return "—";
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return iso;
  const diff = d.getTime() - Date.now();
  if (diff <= 0) return `${d.toLocaleString()} (已过期)`;
  const hours = Math.floor(diff / 3_600_000);
  const minutes = Math.floor((diff % 3_600_000) / 60_000);
  return `${d.toLocaleString()} (剩 ${hours}h ${minutes}m)`;
}

// ── 响应式布局 ──────────────────────────────────────────────────────────────
//
// 三档断点（与 CSS media query 保持一致）：
//   compact : < 480 px  — 紧凑单列，隐藏次要信息
//   normal  : 480–839px — 当前默认单列
//   wide    : ≥ 840 px  — 双栏：左列控制区 + 右列常驻日志

type Layout = "compact" | "normal" | "wide";

function getLayout(w: number): Layout {
  if (w < 480) return "compact";
  if (w >= 840) return "wide";
  return "normal";
}

function useLayout(): Layout {
  const [layout, setLayout] = useState<Layout>(() =>
    getLayout(window.innerWidth),
  );
  useEffect(() => {
    const onResize = () => setLayout(getLayout(window.innerWidth));
    window.addEventListener("resize", onResize);
    return () => window.removeEventListener("resize", onResize);
  }, []);
  return layout;
}

// ── 日志详情面板：分区展示转发全貌 ──────────────────────────────────
//
// fields 中诸如 inboundHeaders / upstreamReqHeaders / reqBody / respHeaders 这类
// 嵌套对象，由 server.go 的 handleProxy 注入。前端在详情面板里把
// 它们拆为两部分：平铺的基本字段（method/path/status…） + 可折叠的嵌套分区。

const SECTION_LABELS: Record<string, string> = {
  inboundHeaders: "入站请求头（客户端 → 代理）",
  upstreamReqHeaders: "转发请求头（代理 → Anthropic）",
  reqBody: "转发请求体",
  respHeaders: "上游响应头（Anthropic → 代理）",
  respEvents: "上游响应 SSE 事件序列",
  respBody: "上游响应体（非 SSE）",
  respBodyRaw: "上游响应原始文本（SSE）",
};

const SECTION_ORDER = [
  "inboundHeaders",
  "upstreamReqHeaders",
  "reqBody",
  "respHeaders",
  "respEvents",
  "respBody",
  "respBodyRaw",
];

const BASIC_FIELDS_ORDER = [
  "method",
  "path",
  "model",
  "status",
  "upstreamStatus",
  "duration",
  "attempt",
  "reqBodyBytes",
  "respBodyBytes",
];

// 按预设顺序排序字段，未列出的按字母顺序在后
function sortByPreferred(
  entries: [string, unknown][],
  preferred: string[],
): [string, unknown][] {
  return [...entries].sort(([a], [b]) => {
    const ai = preferred.indexOf(a);
    const bi = preferred.indexOf(b);
    if (ai === -1 && bi === -1) return a.localeCompare(b);
    if (ai === -1) return 1;
    if (bi === -1) return -1;
    return ai - bi;
  });
}

// 分区内容渲染：
//   - reqBody/respBody：JSON 代码块
//   - respEvents：SSE 事件列表，逐条展开（event 名 + data 代码块）
//   - respBodyRaw：原始文本代码块
//   - headers：键值表格
function SectionContent({ name, value }: { name: string; value: unknown }) {
  // 处理空 / null
  if (value === null || value === undefined) {
    return <div className="log-detail-empty">(空)</div>;
  }

  // SSE 事件列表：逐条展开，每条 event 名高亮 + data JSON 块
  if (name === "respEvents" && Array.isArray(value)) {
    if (value.length === 0) {
      return <div className="log-detail-empty">(无事件)</div>;
    }
    return (
      <div className="log-detail-events">
        {value.map((evt, i) => {
          const ev = evt as { event?: string; data?: unknown };
          const evName = ev.event || "(unnamed)";
          return (
            <div key={i} className="log-detail-event">
              <div className="log-detail-event__head">
                <span className="log-detail-event__idx">#{i + 1}</span>
                <span className="log-detail-event__name">{evName}</span>
              </div>
              <pre className="log-detail-code log-detail-event__data">
                {typeof ev.data === "string"
                  ? ev.data
                  : JSON.stringify(ev.data, null, 2)}
              </pre>
            </div>
          );
        })}
      </div>
    );
  }

  // 请求体 / 响应体 / 原始文本：JSON 代码块
  if (name === "reqBody" || name === "respBody" || name === "respBodyRaw") {
    if (typeof value === "string") {
      return <pre className="log-detail-code">{value}</pre>;
    }
    return (
      <pre className="log-detail-code">{JSON.stringify(value, null, 2)}</pre>
    );
  }

  // 请求头 / 响应头：键值表格
  if (typeof value === "object" && !Array.isArray(value)) {
    const entries = Object.entries(value as Record<string, unknown>);
    if (entries.length === 0) {
      return <div className="log-detail-empty">(空)</div>;
    }
    // 按键名字母排序，让 headers 列表稳定有序
    entries.sort(([a], [b]) => a.localeCompare(b));
    return (
      <div className="log-detail-headers">
        {entries.map(([k, v]) => (
          <div key={k} className="log-detail-row log-detail-row--header">
            <span className="log-detail-key log-detail-key--header">{k}</span>
            <span className="log-detail-val log-detail-val--header">
              {String(v)}
            </span>
          </div>
        ))}
      </div>
    );
  }

  // 其他类型兼容处理
  return (
    <pre className="log-detail-code">{JSON.stringify(value, null, 2)}</pre>
  );
}

// 为分区提供“概要”（项数 / 字符数 / 事件数），显示在折叠标题右侧
function sectionSummary(name: string, value: unknown): string {
  if (value === null || value === undefined) return "";
  if (name === "respEvents" && Array.isArray(value)) {
    return `${value.length} 事件`;
  }
  if (name === "reqBody" || name === "respBody" || name === "respBodyRaw") {
    if (typeof value === "string") return `${value.length} chars`;
    try {
      return `${JSON.stringify(value).length} chars`;
    } catch {
      return "";
    }
  }
  if (typeof value === "object" && !Array.isArray(value)) {
    return `${Object.keys(value as object).length} 项`;
  }
  return "";
}

// 完整详情面板
function LogDetailPanel({ fields }: { fields: Record<string, unknown> }) {
  const basicEntries: [string, unknown][] = [];
  const sectionEntries: [string, unknown][] = [];

  Object.entries(fields).forEach(([k, v]) => {
    if (k in SECTION_LABELS) {
      sectionEntries.push([k, v]);
    } else {
      basicEntries.push([k, v]);
    }
  });

  const sortedBasic = sortByPreferred(basicEntries, BASIC_FIELDS_ORDER);
  const sortedSections = sortByPreferred(sectionEntries, SECTION_ORDER);

  return (
    <div className="log-detail">
      {sortedBasic.length > 0 && (
        <div className="log-detail-basic">
          {sortedBasic.map(([k, v]) => (
            <div key={k} className="log-detail-row">
              <span className="log-detail-key">{k}</span>
              <span className="log-detail-val">
                {typeof v === "object" && v !== null
                  ? JSON.stringify(v)
                  : String(v)}
              </span>
            </div>
          ))}
        </div>
      )}

      {sortedSections.map(([k, v]) => (
        <details key={k} className="log-detail-section">
          <summary className="log-detail-section__title">
            <span className="log-detail-section__chevron" aria-hidden>
              ▸
            </span>
            <span className="log-detail-section__label">
              {SECTION_LABELS[k]}
            </span>
            <span className="log-detail-section__summary">
              {sectionSummary(k, v)}
            </span>
          </summary>
          <div
            className="log-detail-section__body"
            onClick={(e) => e.stopPropagation()}
          >
            <SectionContent name={k} value={v} />
          </div>
        </details>
      ))}
    </div>
  );
}

// Toggle 是基于原生 checkbox + CSS 美化出来的滑块开关。
//
// 选择原生 input 而不是 div+role=switch 是为了直接获得键盘可达性、表单语义和无障碍辅助技术支持。
function Toggle({
  checked,
  onChange,
  disabled,
  label,
  hint,
}: {
  checked: boolean;
  onChange: (next: boolean) => void;
  disabled?: boolean;
  label: string;
  hint?: string;
}) {
  return (
    <label className={`toggle ${disabled ? "toggle--disabled" : ""}`}>
      <input
        type="checkbox"
        checked={checked}
        disabled={disabled}
        onChange={(e) => onChange(e.target.checked)}
      />
      <span className="toggle__track" aria-hidden>
        <span className="toggle__thumb" />
      </span>
      <span className="toggle__text">
        <span className="toggle__label">{label}</span>
        {hint && <span className="toggle__hint">{hint}</span>}
      </span>
    </label>
  );
}

function App() {
  const [status, setStatus] = useState<ProxyStatus>({
    running: false,
    addr: "",
    port: 8765,
  });
  const [account, setAccount] = useState<AccountInfo | null>(null);
  const [host, setHost] = useState<string>("127.0.0.1");
  const [port, setPort] = useState<number>(8765);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>("");
  const [logs, setLogs] = useState<LogItem[]>([]);
  const [logsExpanded, setLogsExpanded] = useState<boolean>(false);
  const [expandedLogs, setExpandedLogs] = useState<Set<number>>(new Set());
  const [logRefreshing, setLogRefreshing] = useState(false);
  const [settings, setSettings] = useState<SettingsView | null>(null);
  const [settingsBusy, setSettingsBusy] = useState(false);
  // 用来保存每条日志的稳定 uid（避免刷新时 index 错位）
  const logUidRef = useRef(0);

  // 响应式布局档位与衍生标志
  const layout = useLayout();
  const isWide = layout === "wide";
  // 宽屏时日志常驻展开（不依赖 logsExpanded 状态）
  const effectiveLogsExpanded = isWide || logsExpanded;

  const refreshStatus = useCallback(async () => {
    try {
      const s = await ProxyService.Status();
      setStatus(s);
    } catch (e) {
      // Status 调用失败一般是 wails 通道还没就绪，悄默忽略
    }
  }, []);

  const refreshAccount = useCallback(async () => {
    try {
      const a = await ProxyService.Account();
      setAccount(a);
    } catch (e: any) {
      setError(`读取账户信息失败: ${e?.message ?? e}`);
    }
  }, []);

  const refreshSettings = useCallback(async () => {
    try {
      const s = await ProxyService.GetSettings();
      setSettings(s);
      // 用配置文件里的 LastHost/LastPort 初始化表单（仅在没运行时）
      if (s.lastHost) setHost(s.lastHost);
      if (s.lastPort) setPort(s.lastPort);
    } catch (e: any) {
      setError(`读取设置失败: ${e?.message ?? e}`);
    }
  }, []);

  // 从后端环形缓冲区拉取历史日志并覆盖当前展示（刷新按钮、展开面板时调用）。
  const onRefreshLogs = useCallback(async () => {
    setLogRefreshing(true);
    try {
      const fetched = await ProxyService.GetLogs();
      setLogs(
        (fetched ?? []).map((e) => ({ ...e, _uid: logUidRef.current++ })),
      );
      setExpandedLogs(new Set()); // 刷新后重置展开状态
    } catch (e: any) {
      setError(`获取日志失败: ${e?.message ?? e}`);
    } finally {
      setLogRefreshing(false);
    }
  }, []);

  // 切换单条日志的详情展开状态（以 uid 识别，较 index 更稳定）。
  const toggleLogDetail = useCallback((uid: number) => {
    setExpandedLogs((prev) => {
      const next = new Set(prev);
      if (next.has(uid)) {
        next.delete(uid);
      } else {
        next.add(uid);
      }
      return next;
    });
  }, []);

  // 启动时拉一次状态 + 账户信息 + 用户偏好 + 历史日志，并订阅实时日志事件。
  useEffect(() => {
    refreshStatus();
    refreshAccount();
    refreshSettings();
    onRefreshLogs(); // 拉取后端缓存日志（如果后端已有运行记录）

    const off = Events.On("proxy:log", (event: any) => {
      // wails 事件 payload 结构：{ name, sender, data: LogEntry }
      const entry = (event?.data ?? event) as LogEntry;
      if (!entry) return;
      setLogs((prev) => {
        const newEntry = { ...entry, _uid: logUidRef.current++ };
        const next = [...prev, newEntry];
        if (next.length > MAX_LOG_LINES) {
          return next.slice(next.length - MAX_LOG_LINES);
        }
        return next;
      });
    });

    return () => {
      // wails Events.On 返回的 unsubscribe 函数
      if (typeof off === "function") off();
    };
  }, [refreshStatus, refreshAccount, refreshSettings, onRefreshLogs]);

  // 把当前 settings 中某个布尔字段切换成 next，立即写盘并刷新 UI。
  // 失败时回滚 UI 状态并显示错误。
  const updateSetting = useCallback(
    async (patch: Partial<SettingsInput>) => {
      if (!settings) return;
      setSettingsBusy(true);
      setError("");
      const merged: SettingsInput = new SettingsInput({
        autoStartProxy: settings.autoStartProxy,
        hideOnClose: settings.hideOnClose,
        launchOnBoot: settings.launchOnBoot,
        lastHost: settings.lastHost,
        lastPort: settings.lastPort,
        ...patch,
      });
      try {
        const next = await ProxyService.UpdateSettings(merged);
        setSettings(next);
      } catch (e: any) {
        setError(`保存设置失败: ${e?.message ?? e}`);
        // 后端写失败时本地状态保持原样，不主动刷新（GetSettings 也可能给出错误）
      } finally {
        setSettingsBusy(false);
      }
    },
    [settings],
  );

  const onStart = useCallback(async () => {
    setBusy(true);
    setError("");
    try {
      const s = await ProxyService.Start(host, port);
      setStatus(s);
    } catch (e: any) {
      setError(`启动失败: ${e?.message ?? e}`);
    } finally {
      setBusy(false);
    }
  }, [host, port]);

  const onStop = useCallback(async () => {
    setBusy(true);
    setError("");
    try {
      const s = await ProxyService.Stop();
      setStatus(s);
    } catch (e: any) {
      setError(`停止失败: ${e?.message ?? e}`);
    } finally {
      setBusy(false);
    }
  }, []);

  const onRefreshAccount = useCallback(async () => {
    setBusy(true);
    setError("");
    try {
      const a = await ProxyService.RefreshAccount();
      setAccount(a);
    } catch (e: any) {
      setError(`凭证刷新失败: ${e?.message ?? e}`);
    } finally {
      setBusy(false);
    }
  }, []);

  const baseURL = useMemo(() => {
    if (!status.running || !status.addr) return "";
    // status.addr 形如 "127.0.0.1:8765"
    return `http://${status.addr}`;
  }, [status]);

  return (
    <div className="root">
      <TitleBar
        title="localClaudeCodeProxy"
        extra={
          <span className={`badge ${status.running ? "running" : "stopped"}`}>
            {status.running ? STATUS_LABEL.running : STATUS_LABEL.stopped}
          </span>
        }
      />

      {/*
       * .app 在不同断点下有不同行为：
       *   compact/normal : flex-column，.col 用 display:contents 透明化
       *   wide           : CSS Grid 2栏，.col 变为实体容器
       */}
      <main className={`app app--${layout}`}>
        {/* ── 左列 / 单列：账户 · 代理 · 设置 ── */}
        <div className="col col--main">
          {error && <div className="error-banner">{error}</div>}

          <section className="card">
            <h2 className="card-title">账户</h2>
            {account?.hasCredentials ? (
              <>
                <div className="row">
                  <label>订阅类型</label>
                  <span className="value">
                    {account.subscriptionType || "—"}
                  </span>
                </div>
                <div className="row">
                  <label>过期时间</label>
                  <span className="value">
                    {formatExpire(String(account.expiresAt ?? ""))}
                  </span>
                </div>
                <div className="row">
                  <label>凭证路径</label>
                  <span className="value">{account.path}</span>
                </div>
                <div className="row">
                  <button onClick={onRefreshAccount} disabled={busy}>
                    刷新凭证
                  </button>
                </div>
              </>
            ) : (
              <>
                <div className="help">
                  没有读到 Claude Code 凭证文件。请先用官方 <code>claude</code>{" "}
                  CLI 登录一次，凭证默认写到{" "}
                  <code>%USERPROFILE%\.claude\.credentials.json</code>。
                </div>
                <div className="row">
                  <button onClick={onRefreshAccount} disabled={busy}>
                    重新检查
                  </button>
                </div>
              </>
            )}
          </section>

          <section className="card">
            <h2 className="card-title">代理服务</h2>
            <div className="row">
              <label>监听地址</label>
              <select
                value={host}
                onChange={(e) => setHost(e.target.value)}
                disabled={status.running || busy}
              >
                <option value="127.0.0.1">127.0.0.1（仅本机）</option>
                <option value="0.0.0.0">0.0.0.0（暴露到局域网）</option>
              </select>
            </div>
            <div className="row">
              <label>端口</label>
              <input
                type="number"
                min={1}
                max={65535}
                value={port}
                onChange={(e) => setPort(Number(e.target.value) || 0)}
                disabled={status.running || busy}
              />
            </div>
            <div className="row">
              {status.running ? (
                <button className="danger" onClick={onStop} disabled={busy}>
                  停止
                </button>
              ) : (
                <button
                  className="primary"
                  onClick={onStart}
                  disabled={busy || !account?.hasCredentials}
                >
                  启动
                </button>
              )}
              {status.running && (
                <span className="value">监听于 {status.addr}</span>
              )}
            </div>
            {baseURL && (
              <div className="help">
                将 Anthropic 兼容客户端的 base URL 指向 <code>{baseURL}</code>
                &#xff0c;
                <br />
                它会把 OAuth 订阅信息透明转发到 <code>api.anthropic.com</code>。
                <br />
                示例 (curl)： <code>curl {baseURL}/healthz</code>
              </div>
            )}
          </section>

          <section className="card">
            <h2 className="card-title">应用设置</h2>
            {settings ? (
              <>
                <Toggle
                  label="启动应用时自动开启代理"
                  hint="使用上次成功启动的监听地址与端口"
                  checked={settings.autoStartProxy}
                  disabled={settingsBusy}
                  onChange={(v) => updateSetting({ autoStartProxy: v })}
                />
                <Toggle
                  label="点击关闭按鈕时最小化到托盘"
                  hint="关闭时不退出进程，应用继续在系统托盘后台运行"
                  checked={settings.hideOnClose}
                  disabled={settingsBusy}
                  onChange={(v) => updateSetting({ hideOnClose: v })}
                />
                <Toggle
                  label="开机自动启动本应用"
                  hint={
                    settings.launchOnBootSupported
                      ? "登录系统后随之启动 localClaudeCodeProxy"
                      : "当前平台不支持自动注册开机启动"
                  }
                  checked={settings.launchOnBoot}
                  disabled={settingsBusy || !settings.launchOnBootSupported}
                  onChange={(v) => updateSetting({ launchOnBoot: v })}
                />
                <div className="help">
                  配置文件：<code>{settings.configPath}</code>
                </div>
              </>
            ) : (
              <div className="help">正在加载设置…</div>
            )}
          </section>
        </div>

        {/* ── 右列 / 单列尾：日志 ── */}
        <div className="col col--side">
          <section
            className={`card card--collapsible card--logs ${
              effectiveLogsExpanded ? "is-open" : ""
            }`}
          >
            <header className="collapsible__header">
              {isWide ? (
                /* 宽屏：静态标题，无需点击展开 */
                <div className="collapsible__toggle collapsible__toggle--static">
                  <span className="card-title">日志</span>
                  <span className="collapsible__count">{logs.length}</span>
                </div>
              ) : (
                /* 普通 / 紧凑：可折叠 */
                <button
                  type="button"
                  className="collapsible__toggle"
                  onClick={() => setLogsExpanded((v) => !v)}
                  aria-expanded={logsExpanded}
                >
                  <svg
                    className="collapsible__chevron"
                    width="12"
                    height="12"
                    viewBox="0 0 12 12"
                    aria-hidden
                  >
                    <path
                      d="M3 4.5l3 3 3-3"
                      fill="none"
                      stroke="currentColor"
                      strokeWidth="1.4"
                      strokeLinecap="round"
                      strokeLinejoin="round"
                    />
                  </svg>
                  <span className="card-title">日志</span>
                  <span className="collapsible__count">{logs.length}</span>
                </button>
              )}

              {effectiveLogsExpanded && (
                <>
                  <button
                    className="collapsible__action"
                    onClick={(e) => {
                      e.stopPropagation();
                      onRefreshLogs();
                    }}
                    disabled={logRefreshing}
                    title="从后端重新拉取全量日志"
                  >
                    {logRefreshing ? (
                      <svg
                        className="spin"
                        width="12"
                        height="12"
                        viewBox="0 0 24 24"
                        fill="none"
                        stroke="currentColor"
                        strokeWidth="2.5"
                        strokeLinecap="round"
                      >
                        <path d="M21 12a9 9 0 1 1-6.219-8.56" />
                      </svg>
                    ) : (
                      <svg
                        width="12"
                        height="12"
                        viewBox="0 0 24 24"
                        fill="none"
                        stroke="currentColor"
                        strokeWidth="2.5"
                        strokeLinecap="round"
                        strokeLinejoin="round"
                      >
                        <path d="M21 12a9 9 0 0 0-9-9 9.75 9.75 0 0 0-6.74 2.74L3 8" />
                        <path d="M3 3v5h5" />
                        <path d="M3 12a9 9 0 0 0 9 9 9.75 9.75 0 0 0 6.74-2.74L21 16" />
                        <path d="M16 16h5v5" />
                      </svg>
                    )}
                    刷新
                  </button>
                  <button
                    className="collapsible__action"
                    onClick={(e) => {
                      e.stopPropagation();
                      setLogs([]);
                      setExpandedLogs(new Set());
                      ProxyService.ClearLogs().catch(() => {});
                    }}
                    disabled={logs.length === 0}
                  >
                    清空
                  </button>
                </>
              )}
            </header>

            {effectiveLogsExpanded && (
              <div className="log-pane">
                {logs.length === 0 ? (
                  <div className="empty">暂无日志</div>
                ) : (
                  logs.map((l: LogItem) => {
                    const uid = l._uid;
                    const hasFields =
                      l.fields && Object.keys(l.fields).length > 0;
                    const isOpen = expandedLogs.has(uid);
                    return (
                      <div key={uid} className="log-entry">
                        <div
                          className={`log-line ${l.level}${
                            hasFields ? " has-detail" : ""
                          }${isOpen ? " is-open" : ""}`}
                          onClick={() => hasFields && toggleLogDetail(uid)}
                        >
                          <span className="ts">{l.time?.slice(11, 19)}</span>
                          <span className="lv">{l.level}</span>
                          <span className="msg">{l.message}</span>
                          {hasFields && (
                            <span className="detail-toggle" aria-hidden>
                              {isOpen ? "▲" : "▼"}
                            </span>
                          )}
                        </div>
                        {isOpen && hasFields && (
                          <LogDetailPanel
                            fields={l.fields as Record<string, unknown>}
                          />
                        )}
                      </div>
                    );
                  })
                )}
              </div>
            )}
          </section>
        </div>
      </main>
    </div>
  );
}

export default App;
