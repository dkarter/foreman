const grid = document.querySelector("#agent-grid");
const linkState = document.querySelector("#link-state");
const workingCount = document.querySelector("#working-count");
const attentionCount = document.querySelector("#attention-count");
const attentionStat = document.querySelector("#attention-stat");
const toast = document.querySelector("#toast");
const clock = document.querySelector("#clock");
const closeButton = document.querySelector("#close-button");
const closeDialog = document.querySelector("#close-dialog");
const cancelClose = document.querySelector("#cancel-close");
const confirmClose = document.querySelector("#confirm-close");
const hostCPU = document.querySelector("#host-cpu");
const hostRAM = document.querySelector("#host-ram");
const foremanCPU = document.querySelector("#foreman-cpu");
const foremanRAM = document.querySelector("#foreman-ram");

const agentIcons = {
  opencode: `
    <svg viewBox="0 0 24 24" role="img" aria-label="OpenCode">
      <path d="M22 24H2V0h20zM17 4.8H7v14.4h10z" />
    </svg>`,
  claude: `
    <svg viewBox="0 0 24 24" role="img" aria-label="Claude">
      <path d="M17.3 3.54h-3.67l6.7 16.92H24zM6.7 3.54 0 20.46h3.74l1.37-3.55h7.01l1.37 3.55h3.74L10.54 3.54zm-.38 10.22 2.3-5.94 2.29 5.94z" />
    </svg>`,
  codex: `
    <svg viewBox="0 0 24 24" role="img" aria-label="Codex">
      <path d="m12 2 4.3 2.5 4.3 2.5v10l-4.3 2.5L12 22l-4.3-2.5L3.4 17V7l4.3-2.5L12 2Zm0 4.1L7 9v6l5 2.9 5-2.9V9l-5-2.9Zm0 3.4 2.2 1.25v2.5L12 14.5l-2.2-1.25v-2.5L12 9.5Z" />
    </svg>`,
};

let socket;
let reconnectDelay = 500;
let agents = [];
let agentsFingerprint = "";

function connect() {
  const protocol = location.protocol === "https:" ? "wss:" : "ws:";
  socket = new WebSocket(`${protocol}//${location.host}/ws`);

  socket.addEventListener("open", () => {
    reconnectDelay = 500;
    setLinkState(true);
  });

  socket.addEventListener("message", ({ data }) => {
    const message = JSON.parse(data);
    if (message.type === "state") {
      setLinkState(message.state.connected);
      renderMetrics(message.state.metrics);
      const nextFingerprint = JSON.stringify(message.state.agents);
      if (nextFingerprint !== agentsFingerprint) {
        agents = message.state.agents;
        agentsFingerprint = nextFingerprint;
        render();
      }
    }
    if (message.type === "focusResult") {
      showToast(message.ok ? "Pane focused" : message.error || "Focus failed", message.ok);
    }
  });

  socket.addEventListener("close", () => {
    setLinkState(false);
    window.setTimeout(connect, reconnectDelay);
    reconnectDelay = Math.min(reconnectDelay * 1.8, 8000);
  });
}

function setLinkState(connected) {
  linkState.classList.toggle("online", connected);
  linkState.querySelector("span").textContent = connected ? "LIVE" : "OFFLINE";
}

function renderMetrics(metrics = {}) {
  hostCPU.textContent = formatPercent(metrics.hostCpu);
  hostRAM.textContent = formatPercent(metrics.hostRam);
  foremanCPU.textContent = formatPercent(metrics.foremanCpu);
  foremanRAM.textContent = Number.isFinite(metrics.foremanRamMiB)
    ? `${Math.round(metrics.foremanRamMiB)}M`
    : "--";
}

function formatPercent(value) {
  return Number.isFinite(value) ? `${value.toFixed(value < 10 ? 1 : 0)}%` : "--";
}

function render() {
  const working = agents.filter((agent) => agent.status === "working").length;
  const attention = agents.filter((agent) => agent.status === "blocked").length;
  workingCount.textContent = working;
  attentionCount.textContent = attention;
  attentionStat.classList.toggle("has-attention", attention > 0);

  if (agents.length === 0) {
    grid.replaceChildren(createEmptyState());
    return;
  }

  const existing = new Map(
    [...grid.querySelectorAll(".agent-card")].map((card) => [card.dataset.pane, card]),
  );
  grid.querySelector(".empty-state")?.remove();

  agents.forEach((agent, index) => {
    let card = existing.get(agent.paneId);
    if (!card) {
      card = createAgentCard(agent.paneId);
      animateNewCard(card, index);
    }
    updateAgentCard(card, agent);
    const currentCard = grid.children[index];
    if (currentCard !== card) {
      grid.insertBefore(card, currentCard || null);
    }
    existing.delete(agent.paneId);
  });
  existing.forEach((card) => card.remove());
}

function animateNewCard(card, index) {
  if (window.matchMedia("(prefers-reduced-motion: reduce)").matches) {
    return;
  }
  card.animate([{ transform: "translateY(8px)", opacity: 0 }, {}], {
    duration: 220,
    delay: index * 35,
    fill: "backwards",
  });
}

function createEmptyState() {
  const empty = document.createElement("div");
  empty.className = "empty-state";
  empty.innerHTML = "<span class=\"scanner\"></span><p>No live agents detected</p>";
  return empty;
}

function createAgentCard(paneId) {
  const card = document.createElement("button");
  card.className = "agent-card";
  card.type = "button";
  card.dataset.pane = paneId;
  card.innerHTML = `
    <span class="status-rail"></span>
    <span class="card-copy">
      <span class="card-meta"><b></b><span class="kind-icon"></span></span>
      <span class="card-title"></span>
    </span>
    <span class="status-block"><i></i><strong></strong></span>`;
  card.addEventListener("click", () => focusAgent(card.dataset.pane));
  return card;
}

function updateAgentCard(card, agent) {
  const statuses = ["working", "blocked", "done", "idle"];
  const statusClasses = statuses.map((status) => `status-${status}`);
  const status = statuses.includes(agent.status) ? agent.status : "idle";
  card.classList.remove(...statusClasses);
  card.classList.add(`status-${status}`);
  card.classList.toggle("focused", agent.focused);
  card.querySelector(".card-meta b").textContent = agent.workspace || agent.cwd.split("/").at(-1);
  const icon = card.querySelector(".kind-icon");
  if (icon.dataset.kind !== agent.kind) {
    icon.dataset.kind = agent.kind;
    icon.innerHTML = agentIcons[agent.kind.toLowerCase()] || fallbackIcon(agent.kind);
  }
  card.querySelector(".card-title").textContent = agent.title.replace(
    /^(OC|Claude)\s*[|·-]\s*/i,
    "",
  );
  card.querySelector(".status-block strong").textContent = status;
}

function fallbackIcon(kind) {
  const initial = (kind || "?").slice(0, 1).toUpperCase();
  return `<span aria-label="${escapeHTML(kind)}">${escapeHTML(initial)}</span>`;
}

function focusAgent(paneId) {
  if (!socket || socket.readyState !== WebSocket.OPEN) {
    showToast("Herdr is offline", false);
    return;
  }
  socket.send(JSON.stringify({ type: "focus", paneId }));
  showToast("Focusing pane…", true);
}

function showToast(message, success) {
  toast.textContent = message;
  toast.classList.toggle("error", !success);
  toast.classList.add("visible");
  window.clearTimeout(showToast.timeout);
  showToast.timeout = window.setTimeout(() => toast.classList.remove("visible"), 1600);
}

function escapeHTML(value) {
  const element = document.createElement("span");
  element.textContent = value || "";
  return element.innerHTML;
}

const clockFormatter = new Intl.DateTimeFormat([], {
  hour: "2-digit",
  minute: "2-digit",
  hour12: false,
});

function scheduleClock() {
  clock.textContent = clockFormatter.format(new Date());
  window.setTimeout(scheduleClock, 60000 - (Date.now() % 60000));
}

closeButton.addEventListener("click", () => closeDialog.showModal());
cancelClose.addEventListener("click", () => closeDialog.close());
confirmClose.addEventListener("click", () => {
  confirmClose.disabled = true;
  window.close();
  window.setTimeout(() => {
    confirmClose.disabled = false;
    closeDialog.close();
    showToast("Chromium prevented closing the kiosk", false);
  }, 400);
});

scheduleClock();
connect();
