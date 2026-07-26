import { Component, type ComponentChildren, render } from "preact";
import { useCallback, useEffect, useRef, useState } from "preact/hooks";

type AgentStatus = "working" | "blocked" | "done" | "idle";

interface Agent {
  paneId: string;
  workspace: string;
  kind: string;
  status: AgentStatus;
  title: string;
  cwd: string;
  focused: boolean;
}

interface SystemMetrics {
  cpu: number | null;
  ram: number | null;
  ramUsedBytes: number | null;
  ramTotalBytes: number | null;
}

interface Metrics {
  host: SystemMetrics;
  foreman: SystemMetrics;
  foremanConnected: boolean;
}

interface Settings {
  pollIntervalSeconds: 5 | 10 | 30 | 60;
  compactMode: boolean;
}

interface DashboardState {
  connected: boolean;
  agents: Agent[];
  metrics: Metrics;
  settings: Settings;
}

const emptyMetrics: SystemMetrics = {
  cpu: null,
  ram: null,
  ramUsedBytes: null,
  ramTotalBytes: null,
};

const initialState: DashboardState = {
  connected: false,
  agents: [],
  metrics: { host: emptyMetrics, foreman: emptyMetrics, foremanConnected: false },
  settings: { pollIntervalSeconds: 5, compactMode: false },
};

const agentIcons: Record<string, string> = {
  amp: "amp.png",
  claude: "claude.png",
  cline: "cline-dark.png",
  codex: "codex.png",
  copilot: "copilot-dark.png",
  cursor: "cursor-dark.png",
  gemini: "gemini.png",
  grok: "grok-dark.png",
  hermes: "hermes-dark.png",
  kilo: "kilo-dark.png",
  kimi: "kimi.png",
  mastracode: "mastracode-dark.png",
  opencode: "opencode-dark.png",
  pi: "pi-dark.svg",
  qodercli: "qodercli.png",
};

function useForemanSocket(showToast: (message: string, success: boolean) => void) {
  const [state, setState] = useState(initialState);
  const socket = useRef<WebSocket | undefined>(undefined);

  useEffect(() => {
    let stopped = false;
    let reconnectTimer = 0;
    let reconnectDelay = 500;

    const connect = () => {
      const protocol = location.protocol === "https:" ? "wss:" : "ws:";
      const next = new WebSocket(`${protocol}//${location.host}/ws`);
      socket.current = next;
      next.addEventListener("open", () => {
        reconnectDelay = 500;
      });
      next.addEventListener("message", ({ data }) => {
        const message = JSON.parse(String(data));
        if (message.type === "state") setState(message.state as DashboardState);
        if (message.type === "metrics") {
          setState((current) => ({ ...current, metrics: message.metrics as Metrics }));
        }
        if (message.type === "focusResult") {
          showToast(message.ok ? "Pane focused" : message.error || "Focus failed", message.ok);
        }
        if (message.type === "settingsResult" && !message.ok) {
          showToast(message.error || "Settings failed", false);
        }
      });
      next.addEventListener("close", () => {
        if (stopped) return;
        reconnectTimer = window.setTimeout(connect, reconnectDelay);
        reconnectDelay = Math.min(reconnectDelay * 1.8, 8000);
        setState((current) => ({ ...current, connected: false }));
      });
    };

    connect();
    return () => {
      stopped = true;
      window.clearTimeout(reconnectTimer);
      socket.current?.close();
    };
  }, [showToast]);

  const send = useCallback((message: object) => {
    if (socket.current?.readyState !== WebSocket.OPEN) {
      showToast("Foreman is offline", false);
      return false;
    }
    socket.current.send(JSON.stringify(message));
    return true;
  }, [showToast]);

  const focus = useCallback((paneId: string) => {
    if (send({ type: "focus", paneId })) showToast("Focusing pane...", true);
  }, [send, showToast]);

  const updateSettings = useCallback(
    (settings: Partial<Settings>) => send({ type: "settings", settings }),
    [send],
  );

  return {
    state,
    focus,
    updateSettings,
  };
}

function App() {
  const [toast, setToast] = useState<{ message: string; success: boolean }>();
  const [settingsOpen, setSettingsOpen] = useState(() =>
    new URLSearchParams(location.search).has("settings")
  );
  const [closeOpen, setCloseOpen] = useState(false);
  const toastTimer = useRef(0);

  const showToast = useCallback((message: string, success: boolean) => {
    setToast({ message, success });
    window.clearTimeout(toastTimer.current);
    toastTimer.current = window.setTimeout(() => setToast(undefined), 1600);
  }, []);
  const { state, focus, updateSettings } = useForemanSocket(showToast);

  useEffect(() => {
    document.body.classList.toggle("compact-mode", state.settings.compactMode);
  }, [state.settings.compactMode]);

  return (
    <>
      <main class="shell">
        <Header
          agents={state.agents}
          connected={state.connected}
          onSettings={() => setSettingsOpen(true)}
          onClose={() => setCloseOpen(true)}
        />
        {settingsOpen
          ? (
            <SettingsPanel
              settings={state.settings}
              onChange={updateSettings}
              onDone={() => setSettingsOpen(false)}
            />
          )
          : <AgentGrid agents={state.agents} onFocus={focus} />}
        <Footer metrics={state.metrics} />
      </main>
      {toast && (
        <div class={`toast visible${toast.success ? "" : " error"}`} role="status">
          {toast.message}
        </div>
      )}
      <CloseDialog open={closeOpen} onCancel={() => setCloseOpen(false)} onFailure={showToast} />
    </>
  );
}

function Header(props: {
  agents: Agent[];
  connected: boolean;
  onSettings: () => void;
  onClose: () => void;
}) {
  const working = props.agents.filter((agent) => agent.status === "working").length;
  const attention = props.agents.filter((agent) => agent.status === "blocked").length;
  return (
    <header class="masthead">
      <div class="brand-block">
        <h1>FOREMAN</h1>
        <span class="brand-subtitle">
          <img src="/assets/herdr-small.png" alt="" />HERDR CONTROL
        </span>
      </div>
      <div class="header-controls">
        <div class="telemetry" aria-label="Agent summary">
          <div>
            <strong>{working}</strong>
            <span>ACTIVE</span>
          </div>
          <div class={attention > 0 ? "has-attention" : ""}>
            <strong>{attention}</strong>
            <span>NEEDS INPUT</span>
          </div>
          <div class={`link-state${props.connected ? " online" : ""}`}>
            <i></i>
            <span>{props.connected ? "LIVE" : "OFFLINE"}</span>
          </div>
        </div>
        <button
          class="icon-button"
          type="button"
          aria-label="Open settings"
          onClick={props.onSettings}
        >
          <svg viewBox="0 0 24 24" aria-hidden="true">
            <path d="M19.1 13a7.8 7.8 0 0 0 .05-1 7.8 7.8 0 0 0-.05-1l2.1-1.65-2-3.46-2.55 1.03a7.3 7.3 0 0 0-1.72-1L14.54 3h-4l-.4 2.92a7.3 7.3 0 0 0-1.72 1L5.87 5.89l-2 3.46L5.97 11a7.8 7.8 0 0 0-.05 1 7.8 7.8 0 0 0 .05 1l-2.1 1.65 2 3.46 2.55-1.03a7.3 7.3 0 0 0 1.72 1l.4 2.92h4l.39-2.92a7.3 7.3 0 0 0 1.72-1l2.55 1.03 2-3.46L19.1 13Zm-6.57 2.5a3.5 3.5 0 1 1 0-7 3.5 3.5 0 0 1 0 7Z" />
          </svg>
        </button>
        <button
          class="close-button"
          type="button"
          aria-label="Close Foreman"
          onClick={props.onClose}
        >
          ×
        </button>
      </div>
    </header>
  );
}

interface AgentGridProps {
  agents: Agent[];
  onFocus: (paneId: string) => void;
}

class AgentGrid extends Component<AgentGridProps> {
  shouldComponentUpdate(next: AgentGridProps) {
    return next.agents !== this.props.agents || next.onFocus !== this.props.onFocus;
  }

  render() {
    const { agents, onFocus } = this.props;
    return (
      <section class="agent-grid" aria-live="polite">
        {agents.length === 0
          ? (
            <div class="empty-state">
              <span class="scanner"></span>
              <p>No live agents detected</p>
            </div>
          )
          : agents.map((agent, index) => (
            <AgentCard key={agent.paneId} agent={agent} order={index} onFocus={onFocus} />
          ))}
      </section>
    );
  }
}

function AgentCard(
  { agent, order, onFocus }: { agent: Agent; order: number; onFocus: (paneId: string) => void },
) {
  const card = useRef<HTMLButtonElement>(null);
  useEffect(() => {
    if (window.matchMedia("(prefers-reduced-motion: reduce)").matches) return;
    card.current?.animate([{ transform: "translateY(8px)", opacity: 0 }, {}], {
      duration: 220,
      delay: order * 35,
      fill: "backwards",
    });
  }, []);

  const status: AgentStatus = ["working", "blocked", "done", "idle"].includes(agent.status)
    ? agent.status
    : "idle";
  const workspace = agent.workspace || agent.cwd.split("/").at(-1);
  return (
    <button
      ref={card}
      class={`agent-card status-${status}${agent.focused ? " focused" : ""}`}
      type="button"
      onClick={() => onFocus(agent.paneId)}
    >
      <span class="status-rail"></span>
      <span class="card-copy">
        <span class="card-meta">
          <AgentIcon kind={agent.kind} />
          <b>{workspace}</b>
        </span>
        <span class="card-title">{agent.title.replace(/^(OC|Claude)\s*[|·-]\s*/i, "")}</span>
      </span>
      <span class="status-block">
        <i></i>
        <strong>{status}</strong>
      </span>
    </button>
  );
}

function AgentIcon({ kind }: { kind: string }) {
  const normalized = kind.toLowerCase();
  const filename = agentIcons[normalized];
  return (
    <span class="kind-icon">
      {filename
        ? <img src={`/assets/agents/${filename}`} alt={kind} />
        : <span aria-label={kind || "Agent"}>{(kind || "?").slice(0, 1).toUpperCase()}</span>}
    </span>
  );
}

function SettingsPanel(props: {
  settings: Settings;
  onChange: (settings: Partial<Settings>) => boolean;
  onDone: () => void;
}) {
  return (
    <section class="settings-panel">
      <div class="settings-heading">
        <div>
          <span>CONTROL SURFACE</span>
          <h2>Settings</h2>
        </div>
        <button type="button" onClick={props.onDone}>Done</button>
      </div>
      <SettingRow
        title="Display density"
        description="Compact mode fits up to 15 agents without scrolling."
      >
        <div class="segmented-control">
          {[false, true].map((compact) => (
            <button
              type="button"
              class={props.settings.compactMode === compact ? "active" : ""}
              onClick={() => props.onChange({ compactMode: compact })}
            >
              {compact ? "Compact" : "Standard"}
            </button>
          ))}
        </div>
      </SettingRow>
      <SettingRow
        title="Resource polling"
        description="Shared by the Mac host and Raspberry Pi reporter."
      >
        <div class="segmented-control">
          {([5, 10, 30, 60] as const).map((seconds) => (
            <button
              type="button"
              class={props.settings.pollIntervalSeconds === seconds ? "active" : ""}
              onClick={() => props.onChange({ pollIntervalSeconds: seconds })}
            >
              {seconds}s
            </button>
          ))}
        </div>
      </SettingRow>
      <p class="settings-note">Changes apply immediately and persist on the Herdr host.</p>
    </section>
  );
}

function SettingRow(props: { title: string; description: string; children: ComponentChildren }) {
  return (
    <div class="setting-row">
      <div>
        <strong>{props.title}</strong>
        <span>{props.description}</span>
      </div>
      {props.children}
    </div>
  );
}

function Footer({ metrics }: { metrics: Metrics }) {
  const [time, setTime] = useState(formatTime());
  useEffect(() => {
    const timer = window.setInterval(() => setTime(formatTime()), 30000);
    return () => window.clearInterval(timer);
  }, []);
  return (
    <footer>
      <div class="resource-metrics" aria-label="Resource usage">
        <MachineMetrics label="HOST" metrics={metrics.host} connected />
        <MachineMetrics
          label="FOREMAN"
          metrics={metrics.foreman}
          connected={metrics.foremanConnected}
        />
      </div>
      <time>{time}</time>
    </footer>
  );
}

function MachineMetrics(
  { label, metrics, connected }: { label: string; metrics: SystemMetrics; connected: boolean },
) {
  return (
    <span>
      {label} <b>{connected ? formatPercent(metrics.cpu) : "--"}</b> CPU ·{"  "}
      <b>{connected ? formatMemorySize(metrics.ramUsedBytes, metrics.ramTotalBytes) : "--"}</b>
      {"  "}<b>{connected ? formatPercent(metrics.ram) : "--"}</b> RAM
    </span>
  );
}

function CloseDialog(props: {
  open: boolean;
  onCancel: () => void;
  onFailure: (message: string, success: boolean) => void;
}) {
  const dialog = useRef<HTMLDialogElement>(null);
  useEffect(() => {
    if (props.open && !dialog.current?.open) dialog.current?.showModal();
    if (!props.open && dialog.current?.open) dialog.current.close();
  }, [props.open]);
  const close = () => {
    window.close();
    window.setTimeout(() => {
      props.onCancel();
      props.onFailure("Chromium prevented closing the kiosk", false);
    }, 400);
  };
  return (
    <dialog ref={dialog} class="close-dialog" onClose={props.onCancel}>
      <p>Close Foreman for the day?</p>
      <div>
        <button type="button" onClick={props.onCancel}>Keep open</button>
        <button class="confirm-close" type="button" onClick={close}>Close kiosk</button>
      </div>
    </dialog>
  );
}

function formatPercent(value: number | null) {
  return Number.isFinite(value) ? `${value!.toFixed(value! < 10 ? 1 : 0)}%` : "--";
}

function formatMemorySize(used: number | null, total: number | null) {
  if (!Number.isFinite(used) || !Number.isFinite(total)) return "--";
  return `${formatGiB(used!)}/${formatGiB(total!)}`;
}

function formatGiB(bytes: number) {
  const value = bytes / 1024 ** 3;
  return `${value.toFixed(value < 10 ? 1 : 0)}G`;
}

function formatTime() {
  return new Intl.DateTimeFormat([], { hour: "2-digit", minute: "2-digit", hour12: false }).format(
    new Date(),
  );
}

render(<App />, document.querySelector("#app")!);
