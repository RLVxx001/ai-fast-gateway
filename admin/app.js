const state = {
  view: "overview",
  token: localStorage.getItem("afg_admin_token") || "",
  status: null,
};

const titles = {
  overview: ["概览", "网关运行状态、WS bridge 和 session 轮换概览。"],
  logs: ["日志", "快速过滤 EOF、broken 连接、session 轮换和指定 conn_id。"],
  sessions: ["会话", "查看和处理自动 session 轮换映射。"],
  policy: ["策略", "查看当前生效的桥接、绕过、重试和连接池规则。"],
  pool: ["连接池", "查看上游 WebSocket 连接复用情况。"],
};

const $ = (id) => document.getElementById(id);

function authHeaders() {
  return state.token ? { Authorization: `Bearer ${state.token}` } : {};
}

async function api(path, options = {}) {
  const response = await fetch(`/admin/api${path}`, {
    ...options,
    headers: {
      ...authHeaders(),
      ...(options.headers || {}),
    },
  });
  if (!response.ok) {
    const text = await response.text();
    throw new Error(text || `HTTP ${response.status}`);
  }
  return response.json();
}

function setError(message) {
  const box = $("errorBox");
  if (!message) {
    box.classList.add("hidden");
    box.textContent = "";
    return;
  }
  box.textContent = message;
  box.classList.remove("hidden");
}

function setBusy(busy) {
  $("refreshBtn").disabled = busy;
}

function setView(view) {
  state.view = view;
  document.querySelectorAll(".nav-item").forEach((item) => {
    item.classList.toggle("active", item.dataset.view === view);
  });
  document.querySelectorAll(".view").forEach((section) => {
    section.classList.toggle("active", section.id === `view-${view}`);
  });
  const [title, subtitle] = titles[view];
  $("pageTitle").textContent = title;
  $("pageSubtitle").textContent = subtitle;
  refresh();
}

function badgeText(enabled) {
  return enabled ? "enabled" : "disabled";
}

function renderKV(el, rows) {
  el.innerHTML = rows
    .map(([key, value]) => `<dt>${escapeHTML(key)}</dt><dd>${escapeHTML(String(value))}</dd>`)
    .join("");
}

function renderOverview(status) {
  $("uptime").textContent = status.uptime || "--";
  $("startedAt").textContent = status.started_at || "--";
  $("poolTotal").textContent = String(status.pool?.total_conns ?? 0);
  $("poolBreakdown").textContent = `${status.pool?.idle_conns ?? 0} idle / ${status.pool?.busy_conns ?? 0} busy`;
  $("rotationCount").textContent = String(status.session_rotate?.entries ?? 0);
  $("rotationState").textContent = status.session_rotate?.enabled
    ? `enabled, threshold ${status.session_rotate.threshold}`
    : "disabled";
  $("upstream").textContent = status.upstream || "--";
  $("logFile").textContent = status.log_file || "--";

  const bridgeBadge = $("bridgeBadge");
  bridgeBadge.textContent = badgeText(status.bridge.enabled);
  bridgeBadge.className = `status-dot ${status.bridge.enabled ? "ok" : "warn"}`;
  renderKV($("bridgeKV"), [
    ["path", status.bridge.path],
    ["fallback_http", status.bridge.fallback_http],
    ["max_attempts", status.bridge.max_attempts],
    ["max_request_bytes", status.bridge.max_request_bytes || "disabled"],
    ["first_event_wait", status.bridge.first_event_wait],
    ["debug_frames", status.bridge.debug_frames],
  ]);

  const responsesBadge = $("responsesBadge");
  responsesBadge.textContent = badgeText(status.responses.enabled);
  responsesBadge.className = `status-dot ${status.responses.enabled ? "ok" : "warn"}`;
  renderKV($("responsesKV"), [
    ["fallback_http", status.responses.fallback_http],
    ["max_attempts", status.responses.max_attempts],
    ["max_request_bytes", status.responses.max_request_bytes || "disabled"],
    ["admin_auth", status.admin.auth_required ? "token required" : "open"],
    ["state_file", status.session_rotate.state_file || "--"],
  ]);
}

async function loadLogs() {
  const grep = encodeURIComponent($("logGrep").value.trim());
  const lines = $("logLines").value;
  const data = await api(`/logs?tail=${lines}&grep=${grep}`);
  $("logMeta").textContent = `${data.file || "log"} · ${data.lines.length} lines · ${data.matched ?? 0} matched / ${data.total ?? 0} total`;
  $("logOutput").textContent = data.lines.join("\n") || "No matching log lines.";
}

function logDownloadURL(all = false) {
  const params = new URLSearchParams();
  params.set("tail", $("logLines").value);
  params.set("grep", $("logGrep").value.trim());
  if (all) params.set("all", "true");
  if (state.token) params.set("token", state.token);
  return `/admin/api/logs/download?${params.toString()}`;
}

function downloadLogs(all = false) {
  window.location.href = logDownloadURL(all);
}

function renderSessions(items) {
  const list = $("sessionList");
  if (!items.length) {
    list.innerHTML = `<div class="empty">暂无 session 轮换映射。出现连续首帧失败后这里会自动出现记录。</div>`;
    return;
  }
  list.innerHTML = items
    .map((item) => {
      const rotated = item.rotated ? "ok" : "warn";
      const rotatedText = item.rotated ? "rotated" : "tracking";
      return `
        <article class="list-card">
          <div class="card-head">
            <div>
              <div class="card-title">${escapeHTML(shorten(item.original))}</div>
              <div class="card-subtitle">${escapeHTML(item.key)}</div>
            </div>
            <div class="card-actions">
              <span class="pill ${rotated}">${rotatedText}</span>
              <button class="button ghost" data-action="rotate" data-key="${escapeAttr(item.key)}">强制轮换</button>
              <button class="button danger" data-action="delete" data-key="${escapeAttr(item.key)}">删除</button>
            </div>
          </div>
          <div class="mini-grid">
            <div class="mini"><div class="mini-label">Current</div><div class="mini-value">${escapeHTML(shorten(item.current))}</div></div>
            <div class="mini"><div class="mini-label">Failures</div><div class="mini-value">${item.failures}</div></div>
            <div class="mini"><div class="mini-label">Generation</div><div class="mini-value">${item.generation}</div></div>
            <div class="mini"><div class="mini-label">Updated</div><div class="mini-value">${escapeHTML(item.updated_at || "--")}</div></div>
          </div>
        </article>
      `;
    })
    .join("");
}

function renderPool(pool) {
  const list = $("poolList");
  if (!pool.clients?.length) {
    list.innerHTML = `<div class="empty">当前没有池化连接。请求经过 WS bridge 后会显示在这里。</div>`;
    return;
  }
  list.innerHTML = pool.clients
    .map((client) => {
      const sessionItems = client.sessions || [];
      const sessions = sessionItems
        .map(
          (session) => `
            <div class="mini">
              <div class="mini-label">${escapeHTML(shorten(session.session_key))}</div>
              <div class="mini-value">${session.conns} conns · ${session.idle} idle · ${session.busy} busy</div>
              <div class="card-subtitle">${escapeHTML((session.conn_ids || []).join(", "))}</div>
            </div>
          `
        )
        .join("");
      return `
        <article class="list-card">
          <div class="card-head">
            <div>
              <div class="card-title">${escapeHTML(shorten(client.client_key, 72))}</div>
              <div class="card-subtitle">${client.conns} conns · ${client.idle} idle · ${client.busy} busy · ${client.creating} creating</div>
            </div>
            <span class="pill ok">${sessionItems.length} sessions</span>
          </div>
          <div class="mini-grid">${sessions}</div>
        </article>
      `;
    })
    .join("");
}

function renderPolicy(policy) {
  const fields = policy?.fields || {};
  for (const [key, value] of Object.entries(fields)) {
    const input = $(key);
    if (!input) continue;
    if (input.type === "checkbox") {
      input.checked = Boolean(value);
    } else {
      input.value = Number(value || 0);
    }
  }

  $("policyPersistState").textContent = policy?.runtime_only
    ? "当前修改仅在本进程生效；设置 ADMIN_POLICY_STATE_FILE 后可跨重启保存。"
    : `策略会保存到 ${policy.state_file}`;

  $("openaiPolicyBadge").textContent = fields.openai_responses_ws_enabled ? "enabled" : "disabled";
  $("openaiPolicyBadge").className = `pill ${fields.openai_responses_ws_enabled ? "ok" : "warn"}`;
  $("ccPolicyBadge").textContent = fields.cc_ws_bridge_enabled ? "enabled" : "disabled";
  $("ccPolicyBadge").className = `pill ${fields.cc_ws_bridge_enabled ? "ok" : "warn"}`;

  const readOnly = policy?.read_only || {};
  renderKV($("policyReadOnly"), [
    ["pool_mode", readOnly.pool_mode || "--"],
    ["pool_max_conns", readOnly.pool_max_conns ?? "--"],
    ["pool_max_idle", readOnly.pool_max_idle ?? "--"],
    ["pool_idle_ttl", readOnly.pool_idle_ttl || "--"],
    ["pool_acquire_timeout", readOnly.pool_acquire_timeout || "--"],
  ]);

  const list = $("policyList");
  const items = policy?.items || [];
  if (!items.length) {
    list.innerHTML = `<div class="empty">暂无策略信息。</div>`;
    return;
  }
  list.innerHTML = items
    .map(
      (item) => `
        <article class="policy-card">
          <div class="policy-top">
            <div class="card-title">${escapeHTML(item.name)}</div>
            <span class="pill ${item.status.includes("enabled") || item.status.includes("bytes") ? "ok" : "warn"}">${escapeHTML(item.status)}</span>
          </div>
          <p>${escapeHTML(item.description)}</p>
        </article>
      `
    )
    .join("");
}

function collectPolicyFields() {
  const boolIDs = [
    "openai_responses_ws_enabled",
    "openai_responses_ws_fallback",
    "cc_ws_bridge_enabled",
    "cc_ws_bridge_fallback",
    "cc_ws_session_rotate_enabled",
    "cc_ws_bridge_debug_frames",
  ];
  const intIDs = [
    "cc_ws_first_event_timeout_ms",
    "openai_responses_ws_max_request_bytes",
    "openai_responses_ws_max_attempts",
    "cc_ws_bridge_max_request_bytes",
    "cc_ws_bridge_max_attempts",
    "cc_ws_session_rotate_threshold",
    "ws_debug_payload_bytes",
  ];
  const payload = {};
  payload.upstream_url = $("upstream_url").value.trim();
  payload.cc_ws_bridge_path = $("cc_ws_bridge_path").value.trim();
  boolIDs.forEach((id) => {
    payload[id] = $(id).checked;
  });
  intIDs.forEach((id) => {
    payload[id] = Number($(id).value || 0);
  });
  return payload;
}

async function savePolicy(event) {
  event.preventDefault();
  const button = $("savePolicyBtn");
  button.disabled = true;
  setError("");
  try {
    const updated = await api("/policy", {
      method: "PATCH",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(collectPolicyFields()),
    });
    renderPolicy(updated);
    await refresh();
  } catch (err) {
    setError(cleanError(err.message));
  } finally {
    button.disabled = false;
  }
}

async function refresh() {
  setBusy(true);
  setError("");
  try {
    const status = await api("/status");
    state.status = status;
    renderAuth(status);
    renderOverview(status);
    if (state.view === "logs") {
      await loadLogs();
    }
    if (state.view === "sessions") {
      renderSessions(await api("/session-rotations"));
    }
    if (state.view === "policy") {
      renderPolicy(await api("/policy"));
    }
    if (state.view === "pool") {
      renderPool(await api("/pool"));
    }
  } catch (err) {
    setError(cleanError(err.message));
    if (err.message.includes("admin token required")) {
      $("tokenPanel").classList.remove("hidden");
    }
  } finally {
    setBusy(false);
  }
}

function renderAuth(status) {
  const el = $("authState");
  if (!status.admin.auth_required) {
    el.textContent = "open";
    el.className = "pill warn";
    return;
  }
  el.textContent = state.token ? "token set" : "token needed";
  el.className = `pill ${state.token ? "ok" : "warn"}`;
}

function cleanError(message) {
  try {
    const parsed = JSON.parse(message);
    return parsed.error || message;
  } catch {
    return message;
  }
}

function shorten(value, max = 44) {
  value = String(value || "--");
  if (value.length <= max) return value;
  return `${value.slice(0, Math.max(8, max - 12))}...${value.slice(-8)}`;
}

function escapeHTML(value) {
  return String(value)
    .replaceAll("&", "&amp;")
    .replaceAll("<", "&lt;")
    .replaceAll(">", "&gt;")
    .replaceAll('"', "&quot;")
    .replaceAll("'", "&#039;");
}

function escapeAttr(value) {
  return escapeHTML(value);
}

document.querySelectorAll(".nav-item").forEach((item) => {
  item.addEventListener("click", () => setView(item.dataset.view));
});

$("refreshBtn").addEventListener("click", refresh);
$("loadLogsBtn").addEventListener("click", loadLogs);
$("downloadLogsBtn").addEventListener("click", () => downloadLogs(false));
$("downloadAllLogsBtn").addEventListener("click", () => downloadLogs(true));
$("logGrep").addEventListener("keydown", (event) => {
  if (event.key === "Enter") loadLogs();
});
$("tokenBtn").addEventListener("click", () => $("tokenPanel").classList.toggle("hidden"));
$("saveTokenBtn").addEventListener("click", () => {
  state.token = $("tokenInput").value.trim();
  localStorage.setItem("afg_admin_token", state.token);
  refresh();
});
$("clearTokenBtn").addEventListener("click", () => {
  state.token = "";
  $("tokenInput").value = "";
  localStorage.removeItem("afg_admin_token");
  refresh();
});
$("sessionList").addEventListener("click", async (event) => {
  const button = event.target.closest("button[data-action]");
  if (!button) return;
  const key = button.dataset.key;
  button.disabled = true;
  try {
    if (button.dataset.action === "delete") {
      await api(`/session-rotations/${encodeURIComponent(key)}`, { method: "DELETE" });
    }
    if (button.dataset.action === "rotate") {
      await api(`/session-rotations/${encodeURIComponent(key)}/rotate`, { method: "POST" });
    }
    renderSessions(await api("/session-rotations"));
  } catch (err) {
    setError(cleanError(err.message));
  } finally {
    button.disabled = false;
  }
});
$("policyForm").addEventListener("submit", savePolicy);

$("tokenInput").value = state.token;
refresh();
