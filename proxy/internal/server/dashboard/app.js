"use strict";

const $ = (id) => document.getElementById(id);
const number = (n) =>
  n == null ? "—" : new Intl.NumberFormat("zh-CN").format(n);
const money = (n) =>
  n == null
    ? "—"
    : "$" + Number(n).toFixed(n !== 0 && Math.abs(n) < 0.01 ? 6 : 4);
const duration = (ms) =>
  ms < 1000 ? Math.round(ms) + " ms" : (ms / 1000).toFixed(2) + " s";
const date = (value) => {
  if (!value) return "未提供";
  const d = new Date(value);
  return Number.isNaN(d.valueOf())
    ? "未提供"
    : d.toLocaleString("zh-CN", { hour12: false });
};
let auth = "",
  endpoint = "llm",
  timer,
  paused = false,
  loading = false,
  instance = "",
  previous = null;
let generation = 0;
const samples = [],
  usageCache = new Map(),
  busyKeys = new Set(),
  keyCards = new Map(),
  controllers = new Set();

function node(tag, className, text) {
  const n = document.createElement(tag);
  if (className) n.className = className;
  if (text != null) n.textContent = text;
  return n;
}
function banner(message) {
  $("banner").textContent = message;
  $("banner").hidden = !message;
}

async function api(path) {
  const controller = new AbortController();
  controllers.add(controller);
  const timeout = setTimeout(() => controller.abort(), 18000);
  try {
    const res = await fetch(new URL(path, document.baseURI), {
      headers: { Authorization: "Bearer " + auth },
      signal: controller.signal,
      cache: "no-store",
      credentials: "omit",
    });
    if (res.status === 401) throw new Error("访问密钥无效，请重新登录。");
    if (!res.ok)
      throw new Error(
        "暂时无法获取数据（HTTP " + res.status + "）。稍后重试。",
      );
    return await res.json();
  } finally {
    clearTimeout(timeout);
    controllers.delete(controller);
  }
}

function resetActivity() {
  previous = null;
  samples.length = 0;
}

function drawActivity(data) {
  const now = new Date(data.now).valueOf();
  let rate = 0;
  if (
    previous &&
    now > previous.time &&
    data.totals.requests >= previous.count
  ) {
    rate =
      ((data.totals.requests - previous.count) * 60000) / (now - previous.time);
  }
  previous = { time: now, count: data.totals.requests };
  samples.push({ time: now, rate });
  while (samples.length > 61 || samples[0].time < now - 300000) samples.shift();
  const max = Math.max(1, ...samples.map((s) => s.rate));
  const points = samples.map(
    (s) =>
      (((s.time - now + 300000) / 300000) * 800).toFixed(1) +
      "," +
      (98 - (s.rate / max) * 85).toFixed(1),
  );
  if (points.length === 1) points.unshift("0,98");
  $("activity-line").setAttribute("points", points.join(" "));
  $("activity-rate").textContent = rate.toFixed(rate < 10 ? 1 : 0);
}

function summaryCell(label, value) {
  const n = node("div");
  n.append(node("strong", "", value), node("span", "", label));
  return n;
}
function line(label, value) {
  const n = node("div", "usage-line");
  n.append(node("span", "", label), node("span", "", value));
  return n;
}

function windowMeter(label, usage) {
  const wrap = node("div", "window" + (usage.exceeded ? " exceeded" : ""));
  wrap.append(line(label, money(usage.used) + " / " + money(usage.cap)));
  if (usage.used != null && usage.cap > 0) {
    const bar = node("progress");
    bar.max = usage.cap;
    bar.value = Math.min(usage.used, usage.cap);
    bar.setAttribute("aria-label", label);
    wrap.append(bar);
  }
  wrap.append(
    node(
      "p",
      "fine",
      usage.resetAt ? "重置于 " + date(usage.resetAt) : "尚无重置时间",
    ),
  );
  return wrap;
}

function renderUsage(id) {
  const card = keyCards.get(id),
    item = usageCache.get(id);
  if (!card) return;
  const area = card.querySelector(".usage-body"),
    balance = card.querySelector(".balance");
  const button = card.querySelector("button");
  button.disabled = busyKeys.has(id);
  button.textContent = busyKeys.has(id)
    ? "查询中…"
    : item
      ? "刷新额度"
      : "查询额度";
  area.replaceChildren();
  if (!item) {
    area.hidden = true;
    return;
  }
  area.hidden = false;
  if (item.error) {
    balance.textContent = "—";
    area.append(node("p", "error", item.error));
    return;
  }
  const u = item.data,
    c = u.credits,
    sub = u.subscription,
    sum = u.summary;
  balance.textContent =
    c &&
    [c.monthlyCredits, c.purchasedCredits, c.freeCredits].every(
      (v) => v != null,
    )
      ? money(c.monthlyCredits + c.purchasedCredits + c.freeCredits)
      : "—";
  if (c) {
    area.append(
      line(
        "月度 / 购买 / 免费",
        [c.monthlyCredits, c.purchasedCredits, c.freeCredits]
          .map(money)
          .join(" / "),
      ),
    );
  }
  if (sub) {
    area.append(
      line("套餐", sub.planId || "未提供"),
      line("订阅状态", sub.status || "未提供"),
      line(
        sub.cancelAtPeriodEnd ? "本期结束" : "本期续订",
        date(sub.currentPeriodEnd),
      ),
    );
  }
  if (sum) {
    area.append(
      line(
        sum.periodBasis === "billing-period"
          ? "账期请求 / 费用"
          : "上游汇总请求 / 费用",
        number(sum.totalCount) + " / " + money(sum.totalCost),
      ),
    );
    area.append(
      line(
        "上游输入 / 输出 Token",
        number(sum.totalTokensIn) + " / " + number(sum.totalTokensOut),
      ),
    );
    area.append(
      line(
        "上游成功 / 失败",
        number(sum.completedCount) + " / " + number(sum.failedCount),
      ),
    );
  }
  if (u.windowLimits?.limited) {
    if (u.windowLimits.fiveHour)
      area.append(windowMeter("5 小时额度", u.windowLimits.fiveHour));
    if (u.windowLimits.weekly)
      area.append(windowMeter("每周额度", u.windowLimits.weekly));
  }
  for (const limit of u.orgLimits || []) {
    area.append(
      windowMeter(limit.modelLabel || limit.model || "组织额度", {
        used: limit.spent,
        cap: limit.limit,
        resetAt: limit.resetAt,
        exceeded: limit.exceeded,
      }),
    );
  }
  if (u.errors?.length)
    area.append(
      node(
        "p",
        "error",
        "部分数据不可用：" +
          u.errors
            .map((e) => e.section + (e.status ? " (" + e.status + ")" : ""))
            .join("、"),
      ),
    );
  area.append(node("p", "fine", "上游账号口径 · 更新于 " + date(u.fetchedAt)));
}

async function loadUsage(id) {
  if (busyKeys.has(id)) return;
  if (busyKeys.size >= 2) {
    banner("正在查询其他账号，请稍候再试。");
    return;
  }
  const epoch = generation;
  busyKeys.add(id);
  renderUsage(id);
  try {
    const data = await api("api/keys/" + encodeURIComponent(id) + "/usage");
    if (epoch === generation && auth) usageCache.set(id, { data });
  } catch (e) {
    if (epoch === generation && auth)
      usageCache.set(id, {
        error: e.name === "AbortError" ? "查询超时，请稍后重试。" : e.message,
      });
  } finally {
    if (epoch === generation) {
      busyKeys.delete(id);
      renderUsage(id);
    }
  }
}

function renderKeys(data) {
  if (data.authMode === "passthrough") {
    $("key-grid").replaceChildren(
      node(
        "div",
        "panel muted",
        "当前为透传模式，使用下游传入的 key。面板不保留这些凭据，不查询其余额；请求记录归入 passthrough。",
      ),
    );
    $("key-count").textContent = "透传模式";
    return;
  }
  const available = data.routing.keys.filter((k) => k.available).length;
  $("key-count").textContent =
    available + " / " + data.routing.keys.length + " 本地可选";
  const ids = new Set(data.routing.keys.map((k) => k.id));
  for (const [id, card] of keyCards) {
    if (!ids.has(id)) {
      card.remove();
      keyCards.delete(id);
      usageCache.delete(id);
    }
  }
  for (const key of data.routing.keys) {
    let card = keyCards.get(key.id);
    if (!card) {
      card = node("article", "key-card");
      const top = node("div", "key-top");
      top.append(
        node("span", "key-icon", "⌘"),
        node("h3", "key-name", key.id),
        node("span", "key-status"),
      );
      const credit = node("div", "key-credit"),
        amount = node("div");
      amount.append(
        node("p", "fine", "当前账号余额 · USD"),
        node("strong", "balance", "—"),
      );
      const button = node("button", "quiet", "查询额度");
      button.addEventListener("click", () => loadUsage(key.id));
      credit.append(amount, button);
      card.append(
        top,
        node("p", "key-sub"),
        node("div", "key-summary"),
        credit,
        node("div", "usage-body"),
      );
      keyCards.set(key.id, card);
      $("key-grid").append(card);
    }
    const status = card.querySelector(".key-status");
    status.className = "key-status" + (key.available ? "" : " unavailable");
    status.textContent = key.probing
      ? "恢复探测中"
      : key.available
        ? "本地可选"
        : "冷却中";
    const a = data.keys.find((k) => k.id === key.id);
    card.querySelector(".key-sub").textContent =
      key.id === data.routing.next
        ? "预计下一路由 · " + number(a?.inFlight || 0) + " 个在途请求"
        : key.retryAt
          ? "冷却至 " + date(key.retryAt)
          : "按配置顺序备用 · " + number(a?.inFlight || 0) + " 个在途请求";
    card
      .querySelector(".key-summary")
      .replaceChildren(
        summaryCell(
          "完成 / 尝试",
          number(a?.requests || 0) + " / " + number(a?.attempts || 0),
        ),
        summaryCell(
          "本轮 Token",
          number((a?.tokens.input || 0) + (a?.tokens.output || 0)),
        ),
        summaryCell("本轮报告费用", a?.costReports ? money(a.costUSD) : "—"),
      );
    renderUsage(key.id);
  }
}

function cell(row, value, sub) {
  const c = node("td", "", value);
  if (sub) c.append(node("small", "", sub));
  row.append(c);
  return c;
}
function emptyRows(target, text) {
  const tr = node("tr"),
    td = node("td", "empty", text);
  td.colSpan = 7;
  tr.append(td);
  target.append(tr);
}
function modelCell(row, model, sub) {
  const c = cell(row, "", sub),
    label = node("span", "model-name", model || "未解析模型");
  label.title = model;
  c.prepend(label);
}

function renderTables(data) {
  const models = $("model-rows"),
    recent = $("request-rows");
  models.replaceChildren();
  recent.replaceChildren();
  $("model-count").textContent = data.models.length + " 个分组";
  for (const m of data.models) {
    const tr = node("tr");
    modelCell(tr, m.model);
    cell(tr, number(m.requests) + " / " + number(m.succeeded));
    cell(tr, number(m.tokens.input) + " / " + number(m.tokens.output));
    cell(tr, number(m.tokens.cached) + " / " + number(m.tokens.cacheWrite));
    cell(tr, number(m.tokens.reasoning));
    cell(tr, duration(m.latencyMs / m.requests));
    cell(
      tr,
      m.costReports ? money(m.costUSD) : "—",
      m.costReports + " 次报告费用",
    );
    models.append(tr);
  }
  if (!data.models.length)
    emptyRows(models, "还没有调用记录。首次请求完成后，这里会显示模型消耗。");
  $("recent-note").textContent =
    "LLM / Jev 共用最近 " + data.capacity + " 条内存队列，旧记录自动覆盖。";
  for (const r of data.recent) {
    const tr = node("tr"),
      time = new Date(r.startedAt).toLocaleTimeString("zh-CN", {
        hour12: false,
      });
    const c = cell(tr, time, r.id.slice(-8));
    c.title = r.id;
    modelCell(tr, r.model, r.stream ? "流式" : "非流式");
    cell(
      tr,
      r.keyId || "未发往上游",
      r.attempts
        .map((a) => a.keyId + (a.result === "failed" ? " ×" : ""))
        .join(" → ") +
        (r.omittedAttempts ? " …另 " + r.omittedAttempts + " 次" : ""),
    );
    const result = cell(tr, "");
    result.append(
      node(
        "span",
        "badge " + (r.result === "ok" ? "ok" : "failed"),
        r.result === "ok"
          ? "完成"
          : r.result === "canceled"
            ? "已取消"
            : r.result,
      ),
    );
    cell(tr, duration(r.latencyMs));
    cell(
      tr,
      r.usage
        ? number(r.usage.input) + " / " + number(r.usage.output)
        : "未报告",
    );
    cell(tr, money(r.costUSD));
    recent.append(tr);
  }
  if (!data.recent.length)
    emptyRows(
      recent,
      "请求完成后会显示实际 key、轮换顺序与消耗，不记录对话内容。",
    );
}

function render(data) {
  if (instance && instance !== data.instanceId) {
    generation++;
    for (const c of controllers) c.abort();
    busyKeys.clear();
    resetActivity();
    usageCache.clear();
    banner(
      "当前实例发生变化，统计已切换。多 Pod 部署请固定访问一个实例，本面板不聚合不同实例。",
    );
  }
  instance = data.instanceId;
  const a = data.totals;
  $("scope").textContent =
    "本实例启动于 " + date(data.startedAt) + " · 重启后清零";
  $("version").textContent =
    "VERSION " + (data.version || "dev") + " · " + instance.slice(0, 8);
  $("connection").textContent =
    "更新于 " +
    new Date(data.now).toLocaleTimeString("zh-CN", { hour12: false });
  $("metric-requests").textContent = number(a.requests);
  $("metric-success").textContent =
    "成功 " +
    (a.requests ? ((a.succeeded / a.requests) * 100).toFixed(1) : "0.0") +
    "% · 失败 " +
    number(a.failed) +
    " · 取消 " +
    number(a.canceled);
  $("metric-tokens").textContent = number(a.tokens.input + a.tokens.output);
  $("metric-token-detail").textContent =
    "输入 " + number(a.tokens.input) + " / 输出 " + number(a.tokens.output);
  $("metric-cost").textContent = a.costReports ? money(a.costUSD) : "—";
  $("metric-cost-detail").textContent =
    a.costReports + " 次报告费用 / " + a.requests + " 次请求";
  $("metric-latency").textContent = a.requests
    ? duration(a.latencyMs / a.requests)
    : "—";
  $("metric-inflight").textContent =
    number(a.inFlight) +
    " 个请求进行中 · " +
    number(a.attempts) +
    " 次上游尝试";
  $("next-key").textContent =
    data.authMode === "passthrough"
      ? "下游指定"
      : data.routing.next || "暂无可选 key";
  $("route-description").textContent =
    data.authMode === "passthrough"
      ? "使用请求携带的上游凭据"
      : "按配置顺序使用首个可选 key · " +
        (endpoint === "llm" ? "LLM" : "Jev") +
        " 独立熔断";
  drawActivity(data);
  renderKeys(data);
  renderTables(data);
}

async function refresh(force = false) {
  if (!auth || (paused && !force) || loading || document.hidden) return;
  loading = true;
  const scope = endpoint,
    epoch = generation;
  try {
    const data = await api("api/stats?endpoint=" + scope);
    if (scope === endpoint && epoch === generation && auth) render(data);
  } catch (e) {
    if (epoch === generation && auth) {
      $("connection").textContent = "连接中断";
      banner(
        e.name === "AbortError" ? "连接超时，正在等待下次刷新。" : e.message,
      );
    }
  } finally {
    loading = false;
    if (scope !== endpoint && epoch === generation && auth) refresh(true);
  }
}

$("login-form").addEventListener("submit", async (event) => {
  event.preventDefault();
  auth = $("admin-key").value;
  $("admin-key").value = "";
  $("login-button").disabled = true;
  $("login-error").textContent = "";
  try {
    const data = await api("api/stats?endpoint=" + endpoint);
    $("login").hidden = true;
    $("app").hidden = false;
    render(data);
    timer = setInterval(refresh, 5000);
  } catch (e) {
    auth = "";
    $("login-error").textContent =
      e.name === "AbortError" ? "连接超时，请重试。" : e.message;
  } finally {
    $("login-button").disabled = false;
  }
});
$("logout").addEventListener("click", () => {
  generation++;
  auth = "";
  clearInterval(timer);
  for (const c of controllers) c.abort();
  usageCache.clear();
  busyKeys.clear();
  keyCards.clear();
  $("key-grid").replaceChildren();
  resetActivity();
  instance = "";
  paused = false;
  $("pause").textContent = "暂停刷新";
  $("app").hidden = true;
  $("login").hidden = false;
  banner("");
  $("admin-key").focus();
});
$("pause").addEventListener("click", () => {
  paused = !paused;
  $("pause").textContent = paused ? "继续刷新" : "暂停刷新";
  if (!paused) refresh();
});
$("logout-mobile").addEventListener("click", () => $("logout").click());
for (const link of document.querySelectorAll("nav a")) {
  link.addEventListener("click", () => {
    for (const item of document.querySelectorAll("nav a"))
      item.classList.toggle("active", item === link);
  });
}
for (const scope of ["llm", "jev"]) {
  $("scope-" + scope).addEventListener("click", () => {
    if (endpoint === scope) return;
    endpoint = scope;
    resetActivity();
    for (const option of ["llm", "jev"]) {
      $("scope-" + option).classList.toggle("selected", option === scope);
      $("scope-" + option).setAttribute(
        "aria-pressed",
        String(option === scope),
      );
    }
    refresh(true);
  });
}
document.addEventListener("visibilitychange", () => {
  if (!document.hidden) refresh();
});
