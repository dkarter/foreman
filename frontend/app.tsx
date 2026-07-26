import { gcm } from "@noble/ciphers/aes.js";
import { p256 } from "@noble/curves/nist.js";
import { hmac } from "@noble/hashes/hmac.js";
import { sha256 } from "@noble/hashes/sha2.js";
import { concatBytes } from "@noble/hashes/utils.js";
import { Component, type ComponentChildren, render } from "preact";
import { useCallback, useEffect, useRef, useState } from "preact/hooks";

type AgentStatus = "working" | "blocked" | "done" | "idle";
const agentStatuses: AgentStatus[] = ["working", "blocked", "done", "idle"];

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
  terminalApp: string;
}

interface DashboardState {
  connected: boolean;
  agents: Agent[];
  metrics: Metrics;
  settings: Settings;
}

interface DeviceCredential {
  id: string;
  name: string;
  secret: string;
  pairedAt: string;
  hostId: string;
  hostName: string;
  endpoint?: string;
  transportVersion: number;
  tlsCertSha256: string;
}

const loopback = ["127.0.0.1", "localhost", "::1", "[::1]"].includes(location.hostname);
const kioskController = loopback && location.port === "4041";
const localDashboard = loopback && !kioskController;
const query = new URLSearchParams(location.search);
const selectedHostId = query.get("host") || "";
const preview = query.get("preview");
const previewStatuses: AgentStatus[] = preview === "all"
  ? agentStatuses
  : agentStatuses.includes(preview as AgentStatus)
  ? [preview as AgentStatus]
  : [];
const credentialKey = kioskController && selectedHostId
  ? `foreman.deviceCredential.${selectedHostId}`
  : "foreman.deviceCredential";

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
  settings: { pollIntervalSeconds: 5, compactMode: false, terminalApp: "" },
};

const previewAgents: Agent[] = previewStatuses.map((status, index) => ({
  paneId: `preview-${status}`,
  workspace: "foreman-preview",
  kind: ["opencode", "claude", "codex", "gemini"][index],
  status,
  title: `${status[0].toUpperCase()}${status.slice(1)} agent state`,
  cwd: "/tmp/foreman-preview",
  focused: status === "working",
}));

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
  const [credential, setCredentialState] = useState<DeviceCredential | undefined>(() => {
    try {
      const saved = JSON.parse(localStorage.getItem(credentialKey) || "") as DeviceCredential;
      return kioskController && saved.hostId !== selectedHostId ? undefined : saved;
    } catch {
      return undefined;
    }
  });
  const [restoringCredential, setRestoringCredential] = useState(kioskController && !credential);
  const socket = useRef<WebSocket | undefined>(undefined);

  useEffect(() => {
    if (!restoringCredential) return;
    void fetch(`/credential?host=${encodeURIComponent(selectedHostId)}`)
      .then(async (response) => {
        if (!response.ok) return;
        const saved = await response.json() as DeviceCredential;
        if (saved.hostId !== selectedHostId) return;
        localStorage.setItem(credentialKey, JSON.stringify(saved));
        setCredentialState(saved);
      })
      .finally(() => setRestoringCredential(false));
  }, []);

  useEffect(() => {
    let stopped = false;
    let reconnectTimer = 0;
    let reconnectDelay = 500;

    if (!credential && !localDashboard) return;

    const connect = async () => {
      const protocol = location.protocol === "https:" ? "wss:" : "ws:";
      if (kioskController && credential) await provisionSatellite(credential);
      const authentication = credential ? await signedQuery(credential, "GET", "/ws") : "";
      if (stopped) return;
      const query = new URLSearchParams(authentication);
      if (kioskController) query.set("host", selectedHostId);
      const next = new WebSocket(
        `${protocol}//${location.host}/ws${query.size ? `?${query}` : ""}`,
      );
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
        if (message.type === "terminalResult" && !message.ok) {
          showToast(message.error || "Terminal activation failed", false);
        }
        if (message.type === "settingsResult" && !message.ok) {
          showToast(message.error || "Settings failed", false);
        }
      });
      next.addEventListener("close", async () => {
        if (stopped) return;
        if (credential) {
          try {
            const response = await fetch(
              pairingURL(`/api/pairing/device?id=${encodeURIComponent(credential.id)}`),
            );
            if (response.ok && !(await response.json() as { paired: boolean }).paired) {
              void deprovisionSatellite();
              localStorage.removeItem(credentialKey);
              setCredentialState(undefined);
              setState(initialState);
              return;
            }
          } catch {
            // Keep the credential and retry when the Mac is temporarily unreachable.
          }
        }
        reconnectTimer = window.setTimeout(() => void connect(), reconnectDelay);
        reconnectDelay = Math.min(reconnectDelay * 1.8, 8000);
        setState((current) => ({ ...current, connected: false }));
      });
    };

    void connect();
    return () => {
      stopped = true;
      window.clearTimeout(reconnectTimer);
      socket.current?.close();
    };
  }, [credential, showToast]);

  const setCredential = useCallback((next: DeviceCredential) => {
    localStorage.setItem(credentialKey, JSON.stringify(next));
    setCredentialState(next);
  }, []);

  const forgetCredential = useCallback(() => {
    void deprovisionSatellite();
    localStorage.removeItem(credentialKey);
    socket.current?.close();
    setCredentialState(undefined);
    setState(initialState);
  }, []);

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
    credential,
    restoringCredential,
    setCredential,
    forgetCredential,
    focus,
    updateSettings,
  };
}

function App() {
  const [toast, setToast] = useState<{ message: string; success: boolean }>();
  const [settingsOpen, setSettingsOpen] = useState(() => query.has("settings"));
  const [closeOpen, setCloseOpen] = useState(false);
  const toastTimer = useRef(0);

  const showToast = useCallback((message: string, success: boolean) => {
    setToast({ message, success });
    window.clearTimeout(toastTimer.current);
    toastTimer.current = window.setTimeout(() => setToast(undefined), 1600);
  }, []);
  const {
    state,
    credential,
    restoringCredential,
    setCredential,
    forgetCredential,
    focus,
    updateSettings,
  } = useForemanSocket(showToast);
  const agents = previewAgents.length > 0 ? previewAgents : state.agents;

  useEffect(() => {
    document.body.classList.toggle("compact-mode", state.settings.compactMode);
  }, [state.settings.compactMode]);

  useEffect(() => {
    if (kioskController && !selectedHostId && previewAgents.length === 0) {
      location.assign("/choose");
    }
  }, []);

  return (
    <>
      <main class="shell">
        <Header
          agents={agents}
          connected={state.connected}
          onSettings={() => setSettingsOpen(true)}
          onClose={() => setCloseOpen(true)}
        />
        {previewAgents.length > 0
          ? <AgentGrid agents={agents} onFocus={() => {}} />
          : restoringCredential
          ? (
            <section class="pairing-panel">
              <span class="pairing-eyebrow">SECURE DEVICE LINK</span>
              <h2>Restoring pairing...</h2>
            </section>
          )
          : !credential && !localDashboard
          ? <PairingPanel onPaired={setCredential} />
          : settingsOpen
          ? (
            <SettingsPanel
              settings={state.settings}
              onChange={updateSettings}
              onDone={() => setSettingsOpen(false)}
              onForget={forgetCredential}
              onSwitch={() => location.assign("/choose")}
              local={localDashboard}
            />
          )
          : <AgentGrid agents={agents} onFocus={focus} />}
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

  const status: AgentStatus = agentStatuses.includes(agent.status)
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
  onForget: () => void;
  onSwitch: () => void;
  local: boolean;
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
      {!props.local && (
        <SettingRow
          title="Kiosk pairing"
          description="Switch Macs without pairing again, or remove this Mac's local credential."
        >
          <div class="segmented-control">
            <button type="button" onClick={props.onSwitch}>Switch Mac</button>
            <button class="danger-button" type="button" onClick={props.onForget}>Forget</button>
          </div>
        </SettingRow>
      )}
      <p class="settings-note">Changes apply immediately and persist on the Herdr host.</p>
    </section>
  );
}

function PairingPanel({ onPaired }: { onPaired: (credential: DeviceCredential) => void }) {
  const [request, setRequest] = useState<{
    id: string;
    code: string;
    encryptionKey: Uint8Array;
  }>();
  const [status, setStatus] = useState("Pair this kiosk with the Foreman Mac app.");
  const [confirmed, setConfirmed] = useState(false);

  const begin = async () => {
    try {
      setStatus("Creating a secure pairing request...");
      const privateKey = p256.utils.randomSecretKey();
      const clientPublicKey = p256.getPublicKey(privateKey, false);
      const response = await fetch(pairingURL("/api/pairing/request"), {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          name: new URLSearchParams(location.search).get("kiosk") || "Foreman kiosk",
          publicKey: encodeBase64(clientPublicKey),
        }),
      });
      if (!response.ok) throw new Error(await response.text());
      const pairing = await response.json() as { id: string; serverPublicKey: string };
      const serverPublicKeyBytes = decodeBase64(pairing.serverPublicKey);
      const shared = p256.getSharedSecret(privateKey, serverPublicKeyBytes, true).slice(1);
      const codeInput = concatBytes(
        new TextEncoder().encode("foreman-pairing-code"),
        clientPublicKey,
        serverPublicKeyBytes,
      );
      const codeDigest = hmac(sha256, shared, codeInput);
      const codeNumber = new DataView(codeDigest.buffer).getUint32(0) % 1_000_000;
      const encryptionMaterial = concatBytes(shared, clientPublicKey, serverPublicKeyBytes);
      const encryptionKey = sha256(encryptionMaterial);
      setRequest({ id: pairing.id, code: String(codeNumber).padStart(6, "0"), encryptionKey });
      setStatus("Compare this code with the Mac, then approve it on both devices.");
    } catch (error) {
      setStatus(error instanceof Error ? error.message.trim() : "Could not start pairing");
    }
  };

  const confirm = async () => {
    if (!request) return;
    const response = await fetch(pairingURL("/api/pairing/confirm"), {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ id: request.id }),
    });
    if (!response.ok) {
      setStatus("The pairing request expired. Start again.");
      setRequest(undefined);
      return;
    }
    setConfirmed(true);
    setStatus("Kiosk approved. Waiting for approval on the Mac...");
  };

  useEffect(() => {
    if (!request || !confirmed) return;
    const poll = window.setInterval(async () => {
      const response = await fetch(
        pairingURL(`/api/pairing/status?id=${encodeURIComponent(request.id)}`),
      );
      if (!response.ok) return;
      const result = await response.json() as {
        status: string;
        credential?: { nonce: string; ciphertext: string };
      };
      if (result.status === "rejected" || result.status === "expired") {
        window.clearInterval(poll);
        setRequest(undefined);
        setConfirmed(false);
        setStatus(result.status === "rejected" ? "The Mac rejected pairing." : "Pairing expired.");
        return;
      }
      if (result.status !== "paired" || !result.credential) return;
      try {
        const plaintext = gcm(
          request.encryptionKey,
          decodeBase64(result.credential.nonce),
        ).decrypt(decodeBase64(result.credential.ciphertext));
        const credential = JSON.parse(new TextDecoder().decode(plaintext)) as DeviceCredential;
        window.clearInterval(poll);
        await fetch(pairingURL("/api/pairing/complete"), {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ id: request.id }),
        });
        if (kioskController) await provisionSatellite(credential);
        onPaired(credential);
      } catch {
        setStatus("The secure pairing response could not be verified.");
      }
    }, 1000);
    return () => window.clearInterval(poll);
  }, [confirmed, onPaired, request]);

  return (
    <section class="pairing-panel">
      <span class="pairing-eyebrow">SECURE DEVICE LINK</span>
      <h2>{request ? "Verify the code" : "Kiosk not paired"}</h2>
      {request && (
        <div class="pairing-code" aria-label={`Pairing code ${request.code}`}>
          {formatCode(request.code)}
        </div>
      )}
      <p>{status}</p>
      {!request && <button type="button" onClick={() => void begin()}>Start pairing</button>}
      {request && !confirmed && (
        <button type="button" onClick={() => void confirm()}>Code matches</button>
      )}
    </section>
  );
}

async function provisionSatellite(credential: DeviceCredential) {
  try {
    await fetch(`/credential?host=${encodeURIComponent(selectedHostId)}`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(credential),
    });
  } catch {
    // The dashboard remains usable if the optional resource reporter is not running.
  }
}

async function deprovisionSatellite() {
  await fetch(`/credential?host=${encodeURIComponent(selectedHostId)}`, { method: "DELETE" }).catch(
    () => {},
  );
}

function pairingURL(path: string) {
  if (!kioskController) return path;
  const url = new URL(path, location.origin);
  url.searchParams.set("host", selectedHostId);
  return `${url.pathname}${url.search}`;
}

async function signedQuery(credential: DeviceCredential, method: string, path: string) {
  const timestamp = String(Math.floor(Date.now() / 1000));
  const nonce = encodeBase64(globalThis.crypto.getRandomValues(new Uint8Array(16)));
  const payload = new TextEncoder().encode(
    `${method}\n${path}\n${credential.id}\n${timestamp}\n${nonce}`,
  );
  const signature = encodeBase64(hmac(sha256, decodeBase64(credential.secret), payload));
  return new URLSearchParams({ device: credential.id, timestamp, nonce, signature }).toString();
}

function encodeBase64(value: Uint8Array) {
  return btoa(String.fromCharCode(...value)).replaceAll("+", "-").replaceAll("/", "_").replace(
    /=+$/,
    "",
  );
}

function decodeBase64(value: string) {
  const padded = value.replaceAll("-", "+").replaceAll("_", "/").padEnd(
    Math.ceil(value.length / 4) * 4,
    "=",
  );
  return Uint8Array.from(atob(padded), (character) => character.charCodeAt(0));
}

function formatCode(code: string) {
  return `${code.slice(0, 3)} ${code.slice(3)}`;
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
  const close = async () => {
    if (kioskController) {
      try {
        const response = await fetch("/close", { method: "POST" });
        if (response.ok) {
          window.setTimeout(() => props.onFailure("Could not close the kiosk", false), 1200);
          return;
        }
      } catch {
        // Fall back to the browser close behavior when the kiosk controller is unavailable.
      }
    }
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
        <button class="confirm-close" type="button" onClick={() => void close()}>
          Close kiosk
        </button>
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
