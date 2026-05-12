// App.tsx 是 localClaudeCodeProxy 的主界面：provider 选择、账户状态、代理启停与实时日志。
//
// 当前支持两类订阅转发：
//   - ChatGPT Plus/Pro / Codex CLI OAuth -> OpenAI/Codex Responses API
//   - Claude Code OAuth -> Anthropic Messages API
import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Events } from "@wailsio/runtime";
import { ProxyService } from "../bindings/localclaudecodeproxy";
import type {
  AccountView,
  LogEntry,
  ProviderInfo,
  ProxyStatus,
  SettingsView,
} from "../bindings/localclaudecodeproxy/models";
import { SettingsInput } from "../bindings/localclaudecodeproxy/models";
import TitleBar from "./TitleBar";

const MAX_LOG_LINES = 220;
const DEFAULT_STATUS: ProxyStatus = {
  running: false,
  addr: "",
  port: 8765,
  provider: "chatgpt",
  providerName: "ChatGPT 订阅",
};

type ProviderID = "chatgpt" | "claude";
type Layout = "compact" | "normal" | "wide";
type LogItem = LogEntry & { _uid: number };

const SECTION_LABELS: Record<string, string> = {
  inboundHeaders: "入站请求头",
  upstreamReqHeaders: "转发请求头",
  reqBody: "转发请求体",
  respHeaders: "上游响应头",
  respEvents: "上游 SSE 事件",
  respBody: "上游响应体",
  respBodyRaw: "上游原始响应",
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
  "provider",
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

function getLayout(w: number): Layout {
  if (w < 520) return "compact";
  if (w >= 920) return "wide";
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

function formatExpire(value: unknown): string {
  const raw = String(value ?? "");
  if (!raw || raw === "0001-01-01T00:00:00Z") return "—";
  const d = new Date(raw);
  if (Number.isNaN(d.getTime())) return raw;
  const diff = d.getTime() - Date.now();
  if (diff <= 0) return `${d.toLocaleString()} (已过期)`;
  const hours = Math.floor(diff / 3_600_000);
  const minutes = Math.floor((diff % 3_600_000) / 60_000);
  return `${d.toLocaleString()} (剩 ${hours}h ${minutes}m)`;
}

function endpointHelp(provider: ProviderID, baseURL: string): string {
  if (!baseURL) return "";
  if (provider === "chatgpt") {
    return `${baseURL}/v1/responses`;
  }
  return `${baseURL}/v1/messages`;
}

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

function sectionSummary(name: string, value: unknown): string {
  if (value === null || value === undefined) return "";
  if (name === "respEvents" && Array.isArray(value)) return `${value.length} 事件`;
  if (typeof value === "object" && !Array.isArray(value)) {
    return `${Object.keys(value as object).length} 项`;
  }
  const text = typeof value === "string" ? value : JSON.stringify(value);
  return text ? `${text.length} chars` : "";
}

function SectionContent({ name, value }: { name: string; value: unknown }) {
  if (value === null || value === undefined) {
    return <div className="log-detail-empty">(空)</div>;
  }
  if (name === "respEvents" && Array.isArray(value)) {
    if (value.length === 0) return <div className="log-detail-empty">(无事件)</div>;
    return (
      <div className="log-detail-events">
        {value.map((evt, i) => {
          const ev = evt as { event?: string; data?: unknown };
          return (
            <div key={i} className="log-detail-event">
              <div className="log-detail-event__head">
                <span className="log-detail-event__idx">#{i + 1}</span>
                <span className="log-detail-event__name">
                  {ev.event || "(unnamed)"}
                </span>
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
  if (typeof value === "object" && !Array.isArray(value)) {
    const entries = Object.entries(value as Record<string, unknown>).sort(
      ([a], [b]) => a.localeCompare(b),
    );
    if (entries.length === 0) return <div className="log-detail-empty">(空)</div>;
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
  return (
    <pre className="log-detail-code">
      {typeof value === "string" ? value : JSON.stringify(value, null, 2)}
    </pre>
  );
}

function LogDetailPanel({ fields }: { fields: Record<string, unknown> }) {
  const basicEntries: [string, unknown][] = [];
  const sectionEntries: [string, unknown][] = [];
  Object.entries(fields).forEach(([k, v]) => {
    if (k in SECTION_LABELS) sectionEntries.push([k, v]);
    else basicEntries.push([k, v]);
  });

  return (
    <div className="log-detail">
      {sortByPreferred(basicEntries, BASIC_FIELDS_ORDER).length > 0 && (
        <div className="log-detail-basic">
          {sortByPreferred(basicEntries, BASIC_FIELDS_ORDER).map(([k, v]) => (
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

      {sortByPreferred(sectionEntries, SECTION_ORDER).map(([k, v]) => (
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
          <div className="log-detail-section__body">
            <SectionContent name={k} value={v} />
          </div>
        </details>
      ))}
    </div>
  );
}

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
  const [providers, setProviders] = useState<ProviderInfo[]>([]);
  const [selectedProvider, setSelectedProvider] =
    useState<ProviderID>("chatgpt");
  const [status, setStatus] = useState<ProxyStatus>(DEFAULT_STATUS);
  const [accounts, setAccounts] = useState<AccountView[]>([]);
  const [settings, setSettings] = useState<SettingsView | null>(null);
  const [host, setHost] = useState("127.0.0.1");
  const [port, setPort] = useState(8765);
  const [busy, setBusy] = useState(false);
  const [settingsBusy, setSettingsBusy] = useState(false);
  const [error, setError] = useState("");
  const [logs, setLogs] = useState<LogItem[]>([]);
  const [logsExpanded, setLogsExpanded] = useState(false);
  const [expandedLogs, setExpandedLogs] = useState<Set<number>>(new Set());
  const [logRefreshing, setLogRefreshing] = useState(false);
  const logUidRef = useRef(0);

  const layout = useLayout();
  const isWide = layout === "wide";
  const effectiveLogsExpanded = isWide || logsExpanded;
  const provider = selectedProvider;
  const activeAccount = accounts.find((a) => a.provider === provider);
  const selectedInfo = providers.find((p) => p.id === provider);

  const refreshStatus = useCallback(async () => {
    try {
      const next = await ProxyService.Status();
      setStatus(next);
      if (next.provider === "chatgpt" || next.provider === "claude") {
        setSelectedProvider(next.provider);
      }
    } catch {
      // Wails runtime 未就绪时会失败，初始化阶段忽略。
    }
  }, []);

  const refreshAccounts = useCallback(async () => {
    try {
      setAccounts((await ProxyService.Accounts()) ?? []);
    } catch (e: any) {
      setError(`读取账户失败: ${e?.message ?? e}`);
    }
  }, []);

  const refreshSettings = useCallback(async () => {
    try {
      const next = await ProxyService.GetSettings();
      setSettings(next);
      if (next.lastProvider === "chatgpt" || next.lastProvider === "claude") {
        setSelectedProvider(next.lastProvider);
      }
      if (next.lastHost) setHost(next.lastHost);
      if (next.lastPort) setPort(next.lastPort);
    } catch (e: any) {
      setError(`读取设置失败: ${e?.message ?? e}`);
    }
  }, []);

  const refreshLogs = useCallback(async () => {
    setLogRefreshing(true);
    try {
      const fetched = await ProxyService.GetLogs();
      setLogs(
        (fetched ?? []).map((e) => ({ ...e, _uid: logUidRef.current++ })),
      );
      setExpandedLogs(new Set());
    } catch (e: any) {
      setError(`获取日志失败: ${e?.message ?? e}`);
    } finally {
      setLogRefreshing(false);
    }
  }, []);

  useEffect(() => {
    ProxyService.Providers()
      .then((items) => setProviders(items ?? []))
      .catch(() => {});
    refreshStatus();
    refreshAccounts();
    refreshSettings();
    refreshLogs();

    const off = Events.On("proxy:log", (event: any) => {
      const entry = (event?.data ?? event) as LogEntry;
      if (!entry) return;
      setLogs((prev) => {
        const next = [...prev, { ...entry, _uid: logUidRef.current++ }];
        return next.length > MAX_LOG_LINES
          ? next.slice(next.length - MAX_LOG_LINES)
          : next;
      });
    });
    return () => {
      if (typeof off === "function") off();
    };
  }, [refreshAccounts, refreshLogs, refreshSettings, refreshStatus]);

  const baseURL = useMemo(() => {
    if (!status.running || !status.addr) return "";
    return `http://${status.addr}`;
  }, [status]);

  const updateSetting = useCallback(
    async (patch: Partial<SettingsInput>) => {
      if (!settings) return;
      setSettingsBusy(true);
      setError("");
      const merged = new SettingsInput({
        autoStartProxy: settings.autoStartProxy,
        hideOnClose: settings.hideOnClose,
        launchOnBoot: settings.launchOnBoot,
        lastProvider: settings.lastProvider,
        lastHost: settings.lastHost,
        lastPort: settings.lastPort,
        ...patch,
      });
      try {
        setSettings(await ProxyService.UpdateSettings(merged));
      } catch (e: any) {
        setError(`保存设置失败: ${e?.message ?? e}`);
      } finally {
        setSettingsBusy(false);
      }
    },
    [settings],
  );

  const onSelectProvider = useCallback(
    (next: ProviderID) => {
      if (status.running) return;
      setSelectedProvider(next);
      updateSetting({ lastProvider: next, lastHost: host, lastPort: port });
    },
    [host, port, status.running, updateSetting],
  );

  const onStart = useCallback(async () => {
    setBusy(true);
    setError("");
    try {
      const next = await ProxyService.Start(provider, host, port);
      setStatus(next);
      await refreshSettings();
    } catch (e: any) {
      setError(`启动失败: ${e?.message ?? e}`);
    } finally {
      setBusy(false);
    }
  }, [host, port, provider, refreshSettings]);

  const onStop = useCallback(async () => {
    setBusy(true);
    setError("");
    try {
      setStatus(await ProxyService.Stop());
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
      const next = await ProxyService.RefreshAccount(provider);
      setAccounts((prev) => [
        ...prev.filter((item) => item.provider !== provider),
        next,
      ]);
    } catch (e: any) {
      setError(`刷新凭证失败: ${e?.message ?? e}`);
      await refreshAccounts();
    } finally {
      setBusy(false);
    }
  }, [provider, refreshAccounts]);

  const onLoginChatGPT = useCallback(async () => {
    setBusy(true);
    setError("");
    try {
      await ProxyService.LoginChatGPT();
      await refreshAccounts();
    } catch (e: any) {
      setError(`ChatGPT 登录失败: ${e?.message ?? e}`);
    } finally {
      setBusy(false);
    }
  }, [refreshAccounts]);

  const canStart = Boolean(activeAccount?.hasCredentials) && !busy;
  const startDisabled = status.running || !canStart;

  return (
    <div className="root">
      <TitleBar
        title="localClaudeCodeProxy"
        extra={
          <span className={`badge ${status.running ? "running" : "stopped"}`}>
            {status.running ? `${status.providerName} 运行中` : "已停止"}
          </span>
        }
      />

      <main className={`app app--${layout}`}>
        <div className="col col--main">
          {error && <div className="error-banner">{error}</div>}

          <section className="card card--hero">
            <div className="hero-head">
              <div>
                <h1>订阅转 API</h1>
                <p>
                  本地 HTTP 代理，按所选 provider 注入 OAuth 凭证并转发请求。
                </p>
              </div>
              <div className="hero-endpoint">
                <span>Base URL</span>
                <code>{baseURL || "启动后显示"}</code>
              </div>
            </div>

            <div className="provider-switch" role="tablist">
              {providers.map((item) => {
                const id = item.id as ProviderID;
                const account = accounts.find((a) => a.provider === item.id);
                const selected = selectedProvider === item.id;
                return (
                  <button
                    key={item.id}
                    type="button"
                    className={`provider-tab ${selected ? "is-selected" : ""}`}
                    onClick={() => onSelectProvider(id)}
                    disabled={status.running}
                  >
                    <span className="provider-tab__name">{item.name}</span>
                    <span className="provider-tab__desc">{item.protocol}</span>
                    <span
                      className={`provider-tab__state ${
                        account?.hasCredentials ? "ok" : "missing"
                      }`}
                    >
                      {account?.hasCredentials ? "已登录" : "未登录"}
                    </span>
                  </button>
                );
              })}
            </div>
          </section>

          <section className="card">
            <div className="section-head">
              <div>
                <h2 className="card-title">账户</h2>
                <p>{selectedInfo?.description}</p>
              </div>
              <button onClick={onRefreshAccount} disabled={busy}>
                刷新
              </button>
            </div>

            {activeAccount?.hasCredentials ? (
              <div className="info-grid">
                <div>
                  <label>凭证来源</label>
                  <span>{activeAccount.sourceLabel || activeAccount.source || "—"}</span>
                </div>
                <div>
                  <label>认证类型</label>
                  <span>{activeAccount.authType || "—"}</span>
                </div>
                <div>
                  <label>账户</label>
                  <span>
                    {activeAccount.email ||
                      activeAccount.accountId ||
                      activeAccount.subscription ||
                      "—"}
                  </span>
                </div>
                <div>
                  <label>过期时间</label>
                  <span>{formatExpire(activeAccount.expiresAt)}</span>
                </div>
                <div className="info-grid__wide">
                  <label>凭证路径</label>
                  <span>{activeAccount.path || "—"}</span>
                </div>
              </div>
            ) : (
              <div className="empty-state">
                {provider === "chatgpt"
                  ? "未读到 ChatGPT/Codex OAuth。可以直接读取 Codex CLI 登录状态，也可以在这里重新登录。"
                  : "未读到 Claude Code OAuth。请先使用官方 claude CLI 登录。"}
              </div>
            )}

            {provider === "chatgpt" && (
              <div className="actions">
                <button
                  className="primary"
                  onClick={onLoginChatGPT}
                  disabled={busy}
                >
                  浏览器登录 ChatGPT
                </button>
                <span className="help">
                  登录成功后写入本应用配置目录；若本机 Codex CLI 已登录，也会自动作为备用来源。
                </span>
              </div>
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
                  disabled={startDisabled}
                >
                  启动
                </button>
              )}
              <span className="value">
                {status.running ? `监听于 ${status.addr}` : "等待启动"}
              </span>
            </div>
            {baseURL && (
              <div className="help">
                将客户端 base URL 指向 <code>{baseURL}</code>。当前入口：
                <code>{endpointHelp(provider, baseURL)}</code>，健康检查：
                <code>{baseURL}/healthz</code>。
              </div>
            )}
          </section>

          <section className="card">
            <h2 className="card-title">应用设置</h2>
            {settings ? (
              <>
                <Toggle
                  label="启动应用时自动开启代理"
                  hint="使用上次选择的 provider、监听地址与端口"
                  checked={settings.autoStartProxy}
                  disabled={settingsBusy}
                  onChange={(v) => updateSetting({ autoStartProxy: v })}
                />
                <Toggle
                  label="点击关闭按钮时最小化到托盘"
                  hint="关闭主窗口后代理继续运行"
                  checked={settings.hideOnClose}
                  disabled={settingsBusy}
                  onChange={(v) => updateSetting({ hideOnClose: v })}
                />
                <Toggle
                  label="开机自动启动本应用"
                  hint={
                    settings.launchOnBootSupported
                      ? "登录系统后自动启动 localClaudeCodeProxy"
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
              <div className="help">正在加载设置...</div>
            )}
          </section>
        </div>

        <div className="col col--side">
          <section
            className={`card card--collapsible card--logs ${
              effectiveLogsExpanded ? "is-open" : ""
            }`}
          >
            <header className="collapsible__header">
              {isWide ? (
                <div className="collapsible__toggle collapsible__toggle--static">
                  <span className="card-title">日志</span>
                  <span className="collapsible__count">{logs.length}</span>
                </div>
              ) : (
                <button
                  type="button"
                  className="collapsible__toggle"
                  onClick={() => setLogsExpanded((v) => !v)}
                  aria-expanded={logsExpanded}
                >
                  <span className="collapsible__chevron">⌄</span>
                  <span className="card-title">日志</span>
                  <span className="collapsible__count">{logs.length}</span>
                </button>
              )}

              {effectiveLogsExpanded && (
                <>
                  <button
                    className="collapsible__action"
                    onClick={refreshLogs}
                    disabled={logRefreshing}
                  >
                    {logRefreshing ? "刷新中" : "刷新"}
                  </button>
                  <button
                    className="collapsible__action"
                    onClick={() => {
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
                  logs.map((l) => {
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
                          onClick={() => {
                            if (!hasFields) return;
                            setExpandedLogs((prev) => {
                              const next = new Set(prev);
                              next.has(uid) ? next.delete(uid) : next.add(uid);
                              return next;
                            });
                          }}
                        >
                          <span className="ts">{l.time?.slice(11, 19)}</span>
                          <span className="lv">{l.level}</span>
                          <span className="msg">{l.message}</span>
                          {hasFields && (
                            <span className="detail-toggle">
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
