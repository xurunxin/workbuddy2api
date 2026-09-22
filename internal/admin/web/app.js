(() => {
  "use strict";
  const $ = (id) => document.getElementById(id);
  const state = {
    csrf: "",
    status: null,
    config: null,
    view: "overview",
    timer: null,
    oauth: null,
    oauthChecking: false,
    oauthNeedsRestore: true,
    oauthCompletedID: "",
    dirtyConfig: false,
    models: {
      uid: "",
      // realm 拉取该账号目录时后端标注的域（"cn" / "global"）：控制台据此提示
      // 当前展示的是中国区还是国际区目录，两区模型不混为一谈。
      realm: "",
      items: [],
      source: "",
      fetchedAt: null,
      expiresAt: null,
      stale: false,
      error: "",
      request: 0,
      loaded: false,
      loading: false,
    },
    modelSearch: "",
    modelKind: "all",
    modelSort: "default",
    // modelGroups 展开态：默认折叠 router/media 两组（档位与图像模型不与普通模型
    // 平铺混列，用户明确要求），点击组头切换。
    modelGroups: { chat: true, router: false, image: false, video: false },
    selectedModelID: "",
    keys: {
      items: [],
      authenticationRequired: false,
      maxKeys: 1000,
      loaded: false,
      request: 0,
      inFlight: false,
    },
    keyToRevoke: null,
    sessionEpoch: 0,
    copyFallback: null,
  };
  const views = ["overview", "accounts", "models", "keys", "usage", "config"];
  const titles = {
    overview: "服务总览",
    accounts: "账号管理",
    models: "模型与积分",
    keys: "API Key",
    config: "服务配置",
    usage: "消耗统计",
  };
  function notice(message, error = false) {
    const el = $("notice");
    el.textContent = message;
    el.className = error ? "notice error" : "notice";
    el.hidden = false;
    window.setTimeout(() => {
      el.hidden = true;
    }, 7000);
  }
  function setLoading(show) {
    $("loading").hidden = !show;
  }
  function clearChildren(el) {
    while (el.firstChild) el.removeChild(el.firstChild);
  }
  function node(tag, text, className) {
    const el = document.createElement(tag);
    if (text !== undefined) el.textContent = text;
    if (className) el.className = className;
    return el;
  }
  function fmtTime(value) {
    if (!value) return "未查询";
    const date = new Date(value);
    return Number.isNaN(date.getTime())
      ? "未知"
      : date.toLocaleString("zh-CN", { hour12: false });
  }
  // fmtRemain 把剩余秒数格式化为 "1时23分" / "4分05秒" / "42秒"（模型限额恢复倒计时）。
  function fmtRemain(sec) {
    sec = Math.max(0, Math.round(Number(sec) || 0));
    if (sec < 60) return `${sec}秒`;
    const totalMin = Math.floor(sec / 60);
    if (totalMin < 60) return `${totalMin}分${String(sec % 60).padStart(2, "0")}秒`;
    const h = Math.floor(totalMin / 60);
    const m = totalMin % 60;
    if (h < 24) return `${h}时${String(m).padStart(2, "0")}分`;
    const d = Math.floor(h / 24);
    return `${d}天${h % 24}时`;
  }
  function duration(s) {
    s = Number(s) || 0;
    const h = Math.floor(s / 3600);
    const m = Math.floor((s % 3600) / 60);
    return h ? `${h} 小时 ${m} 分钟` : `${m} 分钟`;
  }
  // api 封装控制台接口。写操作一律带 CSRF 头与 JSON Content-Type，因为服务端
  // 强制要求 application/json（否则 415），所以 body 由调用方显式给出。
  async function api(path, options = {}) {
    const headers = new Headers(options.headers || {});
    if (options.method && options.method !== "GET") {
      headers.set("Content-Type", "application/json");
      headers.set("X-CSRF-Token", state.csrf);
      if (options.body === undefined) options = { ...options, body: "{}" };
    }
    const resp = await fetch(`/admin/api/${path}`, {
      credentials: "same-origin",
      ...options,
      headers,
    });
    let data = null;
    try {
      data = await resp.json();
    } catch (_) {}
    if (resp.status === 401) {
      const hadSession = Boolean(state.csrf);
      showLogin(hadSession ? "会话已失效，请重新登录" : "");
      throw new Error("会话已失效");
    }
    if (!resp.ok) {
      const error = new Error(
        data && data.error && data.error.message
          ? data.error.message
          : `请求失败（HTTP ${resp.status}）`,
      );
      error.status = resp.status;
      throw error;
    }
    return data;
  }
  function setView(view) {
    if (!views.includes(view)) view = "overview";
    if (!state.csrf) {
      showLogin();
      return;
    }
    state.view = view;
    history.replaceState(null, "", `#${view}`);
    views.forEach((name) => {
      $(`${name}-view`).hidden = name !== view;
    });
    document
      .querySelectorAll(".nav-item")
      .forEach((el) => el.classList.toggle("active", el.dataset.view === view));
    $("page-title").textContent = titles[view];
    $("breadcrumb").textContent =
      view === "overview" ? "运维中心" : "运维中心 / " + titles[view];
    if (view === "config" && !state.config) loadConfig();
    if (view === "models") {
      renderModelAccounts();
      const uid = $("model-account").value;
      if (uid) loadModels(uid);
    }
    if (view === "keys" && !state.keys.loaded) loadKeys();
    if (view === "usage") loadUsage();
  }
  let usageRequest = 0;
  async function loadUsage() {
    const request = ++usageRequest, epoch = state.sessionEpoch;
    $("usage-status").textContent = "正在加载…";
    try {
      const query = new URLSearchParams({from: $("usage-from").value, to: $("usage-to").value});
      const [data, keys] = await Promise.all([api(`usage?${query}`), api("keys")]);
      if (request !== usageRequest || epoch !== state.sessionEpoch) return;
      const zero = () => ({requests: 0, failures: 0, input_tokens: 0, cached_tokens: 0, output_tokens: 0, missing_usage: 0, cache_missing_usage: 0});
      const sum = (a, b) => Object.keys(a).forEach(k => { a[k] += Number(b[k]) || 0; });
      const total = zero(), byKey = new Map(), byDay = new Map();
      (keys.keys || []).forEach(k => byKey.set(k.id, {key: k, totals: zero()}));
      data.rows.forEach(row => {
        sum(total, row);
        if (!byKey.has(row.key_id)) byKey.set(row.key_id, {key: {name: row.key_id === "anonymous" ? "匿名访问" : row.key_id, status: "unknown"}, totals: zero()});
        sum(byKey.get(row.key_id).totals, row);
        if (!byDay.has(row.day)) byDay.set(row.day, zero());
        sum(byDay.get(row.day), row);
      });
      [["requests", "requests"], ["failures", "failures"], ["input", "input_tokens"], ["output", "output_tokens"], ["cached", "cached_tokens"]].forEach(([id, key]) => { $(`usage-${id}`).textContent = total[key].toLocaleString(); });
      const cells = (body, values) => { const tr = node("tr"); values.forEach(v => tr.append(node("td", typeof v === "number" ? v.toLocaleString() : v))); body.append(tr); };
      const values = t => [t.requests, t.failures, t.input_tokens, t.cached_tokens, t.output_tokens, t.missing_usage, t.cache_missing_usage];
      const keyBody = $("usage-keys"), dayBody = $("usage-days"), creditBody = $("usage-credits");
      [keyBody, dayBody, creditBody].forEach(clearChildren);
      [...byKey.values()].sort((a,b) => b.totals.requests-a.totals.requests).forEach(({key, totals}) => cells(keyBody, [key.name + (key.prefix ? ` (${key.prefix})` : ""), key.status === "active" ? "启用" : key.status === "revoked" ? "已废弃" : "—", ...values(totals)]));
      [...byDay.entries()].reverse().forEach(([day, totals]) => cells(dayBody, [day, ...values(totals)]));
      (data.credits || []).forEach(c => cells(creditBody, [c.uid, c.used ?? "未知", c.remain, fmtTime(c.checked_at)]));
      const credits = data.credits || [];
      const knownCredits = credits.filter(c => c.used !== null && c.used !== undefined);
      $("usage-credit-total").textContent = credits.length ? `已查询 ${credits.length} 个账户 · 已知已用积分 ${knownCredits.reduce((n,c) => n + c.used, 0).toLocaleString()} · 剩余积分 ${credits.reduce((n,c) => n + c.remain, 0).toLocaleString()}${knownCredits.length < credits.length ? ` · ${credits.length - knownCredits.length} 个账户已用积分未知` : ""}` : "";
      if (!byKey.size) cells(keyBody, ["暂无 API Key 用量"]);
      if (!byDay.size) cells(dayBody, ["所选日期暂无请求"]);
      if (!(data.credits || []).length) cells(creditBody, ["尚未查询账户积分"]);
      const usageStatus = `缺失用量的请求：${total.missing_usage.toLocaleString()} · 缓存数据未报告的请求：${total.cache_missing_usage.toLocaleString()}`;
      $("usage-status").textContent = data.persistence_error ? `统计尚未成功落盘，重启可能丢失部分数据，请检查存储权限。${usageStatus}` : usageStatus;
    } catch (err) { if (request === usageRequest && epoch === state.sessionEpoch) $("usage-status").textContent = err.message; }
  }
  $("usage-refresh").addEventListener("click", loadUsage);
  ["usage-from", "usage-to"].forEach(id => $(id).addEventListener("change", loadUsage));
  $("usage-credits-refresh").addEventListener("click", async () => {
    const button = $("usage-credits-refresh"); button.disabled = true;
    let failed = 0;
    try {
      const status = await api("status");
      for (const account of status.accounts || []) {
        if (!state.csrf) break;
        try { await api(`accounts/${encodeURIComponent(account.uid)}/credits`, {method: "POST"}); } catch (_) { failed++; }
      }
      if (state.csrf) { await loadUsage(); if (failed) notice(`${failed} 个账户积分查询失败`, true); }
    } catch (err) { notice(err.message, true); } finally { button.disabled = false; }
  });
  function showLogin(message) {
    clearInterval(state.timer);
    stopOAuth();
    state.oauthNeedsRestore = true;
    state.oauthCompletedID = "";
    $("oauth-status").textContent = "";
    state.csrf = "";
    state.sessionEpoch += 1;
    state.status = null;
    state.config = null;
    state.dirtyConfig = false;
    state.models.request += 1;
    state.models.uid = "";
    state.models.items = [];
    state.models.loaded = false;
    state.models.loading = false;
    state.selectedModelID = "";
    state.modelSearch = "";
    state.modelSort = "default";
    state.keys.request += 1;
    state.keys.items = [];
    state.keys.loaded = false;
    state.keys.inFlight = false;
    state.keyToRevoke = null;
    $("password").value = "";
    $("import-json").value = "";
    $("import-file").value = "";
    $("advanced-json").value = "";
    $("model-search").value = "";
    $("model-kind").value = "all";
    $("model-sort").value = "default";
    clearKeySecret();
    $("login-view").hidden = false;
    views.forEach((name) => {
      $(`${name}-view`).hidden = true;
    });
    $("logout-button").hidden = true;
    $("session-label").textContent = "未登录";
    setLoading(false);
    if (message) notice(message, true);
  }
  function showApp() {
    $("login-view").hidden = true;
    $("logout-button").hidden = false;
    setView(state.view);
  }
  function serviceState(reachable, ready) {
    ["service-dot", "side-dot"].forEach((id) => {
      $(id).className = `dot ${reachable ? "good" : "bad"}`;
    });
    $("service-state").textContent = reachable
      ? ready
        ? "服务运行，账号池可用"
        : "服务运行，账号池未就绪"
      : "服务连接异常";
    $("side-status").textContent = reachable
      ? ready
        ? "服务正常"
        : "等待可用账号"
      : "连接异常";
  }
  function renderStatus(data) {
    state.status = data;
    const ready = Boolean(data.ready);
    serviceState(true, ready);
    $("uptime").textContent =
      `已运行 ${duration(data.uptime_seconds)} · ${data.in_flight_full || 0} 个账号达到并发上限`;
    [
      ["total", data.total],
      ["healthy", data.healthy],
      ["cooling", data.cooling],
      ["disabled", data.disabled],
    ].forEach(([key, value]) => {
      $(`metric-${key}`).textContent = value ?? "—";
    });
    const summary = $("overview-pool");
    clearChildren(summary);
    const metrics = [
      [`${data.healthy || 0} 个可用`, "good"],
      [`${data.cooling || 0} 个冷却`, "warn"],
      [`${data.disabled || 0} 个已停用`, "danger"],
      [`${data.in_flight_full || 0} 个并发已满`, ""],
    ];
    metrics.forEach(([text, cls]) =>
      summary.append(node("span", text, `pill ${cls}`)),
    );
    renderAccounts(data.accounts || [], data.credits_checked_at || {});
    renderModelAccounts();
    if (state.view === "models" && !state.models.uid && $("model-account").value) {
      loadModels($("model-account").value);
    }
  }
  // accountBadge 判定账号的展示态。手动停用（manual_disabled）与系统自动禁用
  // （disabled）是**两个独立的位**，文案必须分开——否则运维看到「已停用」会以为
  // 是自己摘的，点「恢复」却没反应（实际要靠 revive 解自动位）。
  function accountBadge(a) {
    if (a.manual_disabled) return ["已临时停用", "manual"];
    if (a.disabled) return ["已禁用", "disabled"];
    if (a.cooling)
      return [a.cool_kind === "hard_credit" ? "积分冷却" : "冷却中", "cooling"];
    return ["健康", ""];
  }
  function renderAccounts(accounts, checked) {
    const body = $("accounts-body");
    clearChildren(body);
    $("accounts-empty").hidden = accounts.length !== 0;
    document.querySelector("#accounts-view .table-wrap").hidden = accounts.length === 0;
    accounts.forEach((a) => {
      const tr = document.createElement("tr");
      const who = node("td");
      const box = node("div", undefined, "account-name");
      box.append(node("strong", a.nickname || "未命名账号"));
      box.append(node("small", a.uid));
      who.append(box);
      tr.append(who);
      const knownCredits = Object.prototype.hasOwnProperty.call(checked, a.uid);
      tr.append(
        node(
          "td",
          knownCredits && Number.isFinite(a.credits)
            ? String(a.credits)
            : Number.isFinite(a.credits) && a.credits > 0
              ? `${a.credits}（缓存）`
              : "未查询",
        ),
      );
      const [label, cls] = accountBadge(a);
      const statusCell = node("td");
      statusCell.append(node("span", label, `status-badge ${cls}`));
      // 两个位同时置位时两行都要显示：运维需要同时看到「我为什么摘它」和
      //「系统为什么判它坏」——合并成一行会互相覆盖，排查时会丢信息。
      if (a.manual_disabled) {
        statusCell.append(
          node("small", a.manual_reason ? `手动摘除：${a.manual_reason}` : "手动摘除", "status-detail manual-line"),
        );
      }
      const detail = a.disabled
        ? a.disabled_reason || a.reason
        : a.cooling
          ? `${a.reason ? `${a.reason} · ` : ""}剩余 ${a.cool_remaining_sec ?? "未知"} 秒`
          : a.reason;
      if (detail) statusCell.append(node("small", detail, "status-detail"));
      // 模型级限额台账（6004）：每模型一行，显示剩余恢复时间——Until/ResetAt 取
      // 更晚者（ResetAt 是上游权威墙钟，Until 可能被 soft_rate_max 截断过）。
      if (Array.isArray(a.rate_limited_models) && a.rate_limited_models.length) {
        const box = node("div", undefined, "rate-limited");
        a.rate_limited_models.forEach((m) => {
          if (!m || !m.model) return;
          const end = [m.reset_at, m.until]
            .filter(Boolean)
            .map((v) => new Date(v))
            .filter((d) => Number.isFinite(d.getTime()))
            .sort((x, y) => y - x)[0];
          let remain = "恢复中";
          if (end) {
            const sec = Math.round((end.getTime() - Date.now()) / 1000);
            remain =
              sec > 0
                ? `剩 ${fmtRemain(sec)}（${fmtTime(end.getTime())} 恢复）`
                : "已到恢复时点";
          }
          const line = node("small", `⛔ ${m.model}：${remain}`, "status-detail rate-limited-line");
          box.append(line);
        });
        if (box.childElementCount) statusCell.append(box);
      }
      tr.append(statusCell);
      tr.append(node("td", String(a.in_flight || 0)));
      tr.append(node("td", knownCredits ? fmtTime(checked[a.uid]) : "未查询"));
      const actions = node("td");
      const actionBox = node("div", undefined, "table-actions");
      const credit = node("button", "刷新积分", "mini-button");
      credit.type = "button";
      credit.addEventListener("click", () => accountAction(a.uid, "credits"));
      actionBox.append(credit);
      // 暂停/恢复：只动 manual_disabled 位。健康号给「临时停用」，已手动摘除的号
      // 给「恢复」。自动禁用的号不显示停用按钮——它已经在池外，再摘一次没有意义。
      if (a.manual_disabled) {
        actionBox.append(accountButton("恢复", "manual_enable", a.uid));
      } else if (!a.disabled) {
        actionBox.append(accountButton("临时停用", "manual_disable", a.uid));
      }
      // 复活：只动系统自动禁用位。与「恢复」正交，两个位都清空账号才回选号池。
      if (a.disabled) {
        actionBox.append(accountButton("复活", "revive", a.uid));
      }
      actions.append(actionBox);
      tr.append(actions);
      body.append(tr);
    });
  }
  // accountButton 生成一个账号级 mini 操作按钮（统一 type/事件绑定，避免每处重复）。
  function accountButton(label, action, uid) {
    const button = node("button", label, "mini-button");
    button.type = "button";
    button.addEventListener("click", () => accountAction(uid, action));
    return button;
  }
  function clearCopyFallback() {
    if (state.copyFallback && state.copyFallback.parentNode) {
      state.copyFallback.parentNode.removeChild(state.copyFallback);
    }
    state.copyFallback = null;
  }
  async function copyText(text, label, fallbackEl) {
    const value = String(text || "");
    if (!value) {
      notice(`${label}为空，无法复制`, true);
      return false;
    }
    clearCopyFallback();
    try {
      if (navigator.clipboard && window.isSecureContext) {
        await navigator.clipboard.writeText(value);
        notice(`${label}已复制`);
        return true;
      }
    } catch (_) {
      // navigator.clipboard is commonly unavailable on an HTTP LAN page.
    }
    if (fallbackEl && "value" in fallbackEl) {
      fallbackEl.value = value;
      fallbackEl.focus();
      fallbackEl.select();
      notice(`无法直接复制${label}，文本已选中，请按 Ctrl+C 完成复制`, true);
      return false;
    }
    const fallback = document.createElement("textarea");
    fallback.className = "copy-fallback-input";
    fallback.value = value;
    fallback.setAttribute("readonly", "");
    fallback.setAttribute("aria-label", `${label}复制备用文本`);
    document.body.append(fallback);
    state.copyFallback = fallback;
    fallback.focus();
    fallback.select();
    notice(`无法直接复制${label}，文本已选中，请按 Ctrl+C 完成复制`, true);
    return false;
  }
  function renderModelAccounts() {
    const select = $("model-account");
    if (!select) return;
    const accounts = (state.status && state.status.accounts) || [];
    const selected = state.models.uid;
    clearChildren(select);
    if (!accounts.length) {
      const option = node("option", "暂无账号");
      option.value = "";
      option.disabled = true;
      option.selected = true;
      select.append(option);
      select.disabled = true;
      $("models-refresh").disabled = true;
      state.models.request += 1;
      state.models.uid = "";
      state.models.items = [];
      state.models.loaded = false;
      state.models.loading = false;
      state.selectedModelID = "";
      clearModelView("暂无账号可查询模型。");
      return;
    }
    select.disabled = false;
    $("models-refresh").disabled = Boolean(state.models.loading);
    accounts.forEach((account) => {
      const option = node("option", `${account.nickname || "未命名账号"} · ${account.uid}`);
      option.value = account.uid;
      select.append(option);
    });
    const next = accounts.some((account) => account.uid === selected) ? selected : accounts[0].uid;
    select.value = next;
    if (state.models.uid && state.models.uid !== next) {
      state.models.request += 1;
      state.models.uid = "";
      state.models.items = [];
      state.models.loaded = false;
      state.models.loading = false;
      state.selectedModelID = "";
      clearModelView("请选择账号后读取模型列表。");
    }
  }
  function clearModelView(message = "选择账号后读取模型列表。") {
    const source = $("model-source");
    if (source) {
      source.textContent = message;
      source.className = "model-source warning";
    }
    const body = $("models-body");
    if (body) clearChildren(body);
    if ($("models-table-wrap")) $("models-table-wrap").hidden = true;
    if ($("models-empty")) {
      $("models-empty").hidden = false;
      const title = $("models-empty").querySelector("strong");
      const detail = $("models-empty").querySelector("p");
      if (title) title.textContent = "暂无模型数据";
      if (detail) detail.textContent = message;
    }
    if ($("model-detail")) $("model-detail").hidden = true;
    if ($("model-realm")) $("model-realm").hidden = true;
    if ($("model-picker")) clearChildren($("model-picker"));
    if ($("model-id")) $("model-id").textContent = "—";
    if ($("model-facts")) clearChildren($("model-facts"));
    if ($("model-example")) $("model-example").textContent = "";
  }
  function formatTokens(value) {
    const number = Number(value);
    if (!Number.isFinite(number) || number <= 0) return "未知";
    return number.toLocaleString("zh-CN");
  }
  function formatMultiplier(model) {
    const multiplier = model && model.credit_multiplier;
    if (typeof multiplier === "number" && Number.isFinite(multiplier)) {
      const raw = String(multiplier);
      if (raw.includes("e")) return `×${multiplier.toLocaleString("en-US", { maximumFractionDigits: 12, minimumFractionDigits: 2 })}`;
      const [integer, fraction = ""] = raw.split(".");
      return `×${integer}.${fraction.padEnd(2, "0")}`;
    }
    if (model && (model.credit_type === "dynamic" || /^(auto|动态)$/i.test(String(model.credits_label || "")))) return "动态";
    return "未知";
  }
  function formatCredit(model) {
    const label = String(model && model.credits_label || "").trim();
    const multiplier = model && model.credit_multiplier;
    // 倍率数值已解析（含显式 0）时不再拼接原文——否则 "×0.03 · x0.03" 同一倍率显示两遍。
    if (typeof multiplier === "number" && Number.isFinite(multiplier)) {
      // 数值 0 且原文也是 0 → 就是限免/免费，交给标签列表达，不重复展示。
      return formatMultiplier(model);
    }
    if (!label || /^(auto|动态)$/i.test(label)) return formatMultiplier(model);
    return `${formatMultiplier(model)} · ${label}`;
  }
  // MEDIA_TAGS 已在「分类」列（kind=image/video）表达的媒体能力标记：
  // 这些 tag 是分类的**来源**而非活动标签，重复展示只会让「活动标签」列变成噪音。
  // craft 同理（上游内部标记，与用户无关，历史实现已过滤）。
  const MEDIA_TAGS = new Set(["craft", "text-to-image", "image-to-image", "text-to-video", "image-to-video"]);
  function modelTags(model) {
    if (!Array.isArray(model && model.tags)) return [];
    const out = [];
    let hasBadge = false;
    model.tags.reduce((tags, raw) => {
      const value = String(raw || "").trim();
      if (!value || MEDIA_TAGS.has(value.toLowerCase())) return tags;
      const badge = value.match(/^badge:([^:]+):#([0-9a-f]{6})$/i);
      if (badge) {
        tags.push({ label: badge[1], color: `#${badge[2].toUpperCase()}` });
        hasBadge = true;
      } else tags.push({ label: value, color: "" });
      return tags;
    }, out);
    // 上游未下发活动 badge 且实测积分为 0 → 推导「限免」标签（免费层可观测，
    // 与成本账本 tier 0 同口径；显式 0 是合法免费观测）。
    const multiplier = model && model.credit_multiplier;
    if (
      !hasBadge &&
      typeof multiplier === "number" &&
      Number.isFinite(multiplier) &&
      multiplier <= 0
    ) {
      out.push({ label: "限免", color: "#16a34a" });
    }
    return out;
  }
  // modelKind 模型分类（与后端 upstream.ModelKind 一一对应）：
  //   chat=普通对话 / router=自动路由档位 / image=图像 / video=视频。
  // 缺 kind 的旧快照一律按 chat 处理（向后兼容，不因字段缺失而丢模型）。
  const MODEL_KINDS = {
    chat: { label: "对话", hint: "普通对话模型，走 /v1/chat/completions 或 /v1/responses" },
    router: { label: "自动路由", hint: "上游按任务自动挑模型的虚拟档位，不是具体模型" },
    image: { label: "图像", hint: "图像模型，走 /v1/images/generations 与 /v1/images/edits" },
    video: { label: "视频", hint: "视频模型，本网关暂无独立入口" },
    completion: { label: "补全", hint: "代码补全 / NES 等非对话专用模型" },
  };
  function modelKindOf(model) {
    const kind = String((model && model.kind) || "").toLowerCase();
    return MODEL_KINDS[kind] ? kind : "chat";
  }
  function modelKindLabel(model) {
    return MODEL_KINDS[modelKindOf(model)].label;
  }
  // kindBadge 分类徽标；router 档位额外显示中文档位名（快速/均衡/极致）。
  function kindBadge(model) {
    const kind = modelKindOf(model);
    const pill = node("span", MODEL_KINDS[kind].label, `pill kind-badge kind-${kind}`);
    pill.title = MODEL_KINDS[kind].hint;
    if (kind === "router" && model.router_tier) {
      const wrap = document.createElement("span");
      wrap.className = "kind-cell";
      wrap.append(pill, node("small", model.router_tier, "kind-tier"));
      return wrap;
    }
    return pill;
  }
  function visibleModels() {
    const query = state.modelSearch.trim().toLowerCase();
    const models = state.models.items.filter((model) => {
      if (state.modelKind !== "all" && modelKindOf(model) !== state.modelKind) return false;
      if (!query) return true;
      return `${model.id || ""} ${model.name || ""} ${model.description || ""}`.toLowerCase().includes(query);
    });
    if (state.modelSort === "default") return models;
    return models.slice().sort((left, right) => {
      const a = typeof left.credit_multiplier === "number" && Number.isFinite(left.credit_multiplier) ? left.credit_multiplier : null;
      const b = typeof right.credit_multiplier === "number" && Number.isFinite(right.credit_multiplier) ? right.credit_multiplier : null;
      if (a === null && b === null) return 0;
      if (a === null) return 1;
      if (b === null) return -1;
      return state.modelSort === "asc" ? a - b : b - a;
    });
  }
  // groupModels 按分类分组（保持后端原序）。用户要求自动路由档位与图像等特殊模型
  // 不与普通模型混列，故分组渲染而非平铺。
  function groupModels(models) {
    const groups = { chat: [], router: [], image: [], video: [], completion: [] };
    models.forEach((m) => { groups[modelKindOf(m)].push(m); });
    return groups;
  }
  function renderModelSource() {
    const out = state.models;
    const sourceLabel = { upstream: "上游最新", cache: "缓存", stale: "过期缓存", unavailable: "不可用" }[out.source] || "未读取";
    const parts = [`来源：${sourceLabel}`];
    if (out.fetchedAt) parts.push(`更新于 ${fmtTime(out.fetchedAt)}`);
    if (out.expiresAt) parts.push(`有效至 ${fmtTime(out.expiresAt)}`);
    if (out.stale) parts.push("当前数据可能已过期");
    if (out.error) parts.push(`失败：${out.error}`);
    $("model-source").textContent = parts.join(" · ");
    $("model-source").className = `model-source ${out.source === "unavailable" || out.stale ? "warning" : ""}`;
    // realm 徽标：明确标注当前展示的是中国区还是国际区账号的目录。
    const badge = $("model-realm");
    if (!badge) return;
    if (!out.realm || !out.items.length) {
      badge.hidden = true;
      return;
    }
    const isGlobal = out.realm === "global";
    badge.hidden = false;
    badge.textContent = isGlobal
      ? "国际区账号（global）：目录来自 workbuddy.ai 上游，仅含国际区模型"
      : "中国区账号（cn）：目录来自 codebuddy.cn 上游，仅含中国区模型";
    badge.className = `realm-badge ${isGlobal ? "realm-global" : "realm-cn"}`;
  }
  function renderModelCapabilities(model) {
    const capabilities = [];
    // 附件能力优先用后端 attachments/supports_attachments（单一事实来源），
    // 后端未给该字段（旧快照）时回落 supports_images。
    const attachments = Array.isArray(model.attachments) ? model.attachments.filter(Boolean) : [];
    const hasImageAttachment = attachments.includes("image") ||
      (attachments.length === 0 && model.supports_attachments === true) ||
      (model.supports_attachments === undefined && model.supports_images === true);
    if (hasImageAttachment) {
      capabilities.push(attachments.length ? `附件：${attachments.join(" / ")}` : "图片");
    }
    if (model.disabled_multimodal) capabilities.push("多模态已关闭");
    if (model.supports_tool_call) capabilities.push("工具");
    if (model.supports_reasoning) {
      const efforts = Array.isArray(model.reasoning_efforts) ? model.reasoning_efforts.filter(Boolean) : [];
      capabilities.push(efforts.length ? `推理：${efforts.join(" / ")}` : "推理");
    }
    if (model.can_disable_thinking) capabilities.push("可关闭思考");
    if (!capabilities.length) {
      const kind = modelKindOf(model);
      if (kind === "image" || kind === "video") return ["媒体模型"];
      return ["基础对话"];
    }
    return capabilities;
  }
  function renderModelDetails(model, visible) {
    const detail = $("model-detail");
    if (!model || !visible.length) {
      detail.hidden = true;
      return;
    }
    detail.hidden = false;
    const picker = $("model-picker");
    clearChildren(picker);
    visible.forEach((item) => {
      const option = node("option", item.name && item.name !== item.id ? `${item.name} · ${item.id}` : item.id);
      option.value = item.id;
      picker.append(option);
    });
    picker.value = model.id;
    $("model-id").textContent = model.id || "未知";
    const facts = $("model-facts");
    clearChildren(facts);
    const fields = [
      ["分类", modelKindLabel(model) + (model.router_tier ? ` · ${model.router_tier}` : "")],
      ["积分倍率", formatCredit(model)],
      ["最大输入", formatTokens(model.max_input_tokens)],
      ["最大输出", formatTokens(model.max_output_tokens)],
      ["能力", renderModelCapabilities(model).join("、")],
    ];
    fields.forEach(([label, value]) => {
      const item = node("div", undefined, "model-fact");
      item.append(node("span", label), node("strong", value));
      facts.append(item);
    });
    if (model.description) facts.append(node("p", model.description, "model-description"));
    renderModelExample(model);
  }
  // modelRow 构造一行模型 <tr>（分组渲染与平铺渲染共用同一套单元格口径）。
  function modelRow(model, selectedID) {
    const row = document.createElement("tr");
    row.tabIndex = 0;
    row.className = model.id === selectedID ? "selected" : "";
    row.setAttribute("aria-selected", model.id === selectedID ? "true" : "false");
    row.addEventListener("click", () => selectModel(model.id));
    row.addEventListener("keydown", (event) => {
      if (event.key === "Enter" || event.key === " ") {
        event.preventDefault();
        selectModel(model.id);
      }
    });
    const identity = node("td");
    identity.append(node("strong", model.name || model.id || "未命名模型"));
    identity.append(node("code", model.id || "未知", "model-row-id"));
    if (model.description) identity.append(node("small", model.description, "model-row-description"));
    row.append(identity);
    const kindCell = node("td", undefined, "model-kind");
    kindCell.append(kindBadge(model));
    row.append(kindCell);
    row.append(node("td", formatMultiplier(model), "model-credit"));
    row.append(node("td", `${formatTokens(model.max_input_tokens)} / ${formatTokens(model.max_output_tokens)}`));
    row.append(node("td", renderModelCapabilities(model).join("、"), "model-capabilities"));
    const tags = node("td", undefined, "model-tags");
    modelTags(model).forEach((tag) => {
      const pill = node("span", tag.label, "pill model-tag-badge");
      if (tag.color) {
        pill.style.color = tag.color;
        pill.style.borderColor = tag.color;
      }
      tags.append(pill);
    });
    if (!tags.childElementCount) tags.append(node("span", "—", "muted"));
    row.append(tags);
    return row;
  }
  // modelGroupRow 分组表头行（可折叠）：档位 / 图像 / 视频 / 补全 各自成组，
  // 组头显示名称、条数与该组用途说明。
  function modelGroupRow(kind, count) {
    const row = document.createElement("tr");
    row.className = `model-group-row group-${kind}`;
    const cell = document.createElement("td");
    cell.colSpan = 6;
    const expanded = Boolean(state.modelGroups[kind]);
    const head = document.createElement("button");
    head.type = "button";
    head.className = "model-group-head";
    head.setAttribute("aria-expanded", expanded ? "true" : "false");
    head.append(node("span", expanded ? "▾" : "▸", "model-group-caret"));
    head.append(node("strong", MODEL_KINDS[kind].label));
    head.append(node("span", `${count} 个`, "model-group-count"));
    head.append(node("small", MODEL_KINDS[kind].hint, "model-group-hint"));
    head.addEventListener("click", (event) => {
      event.stopPropagation();
      state.modelGroups[kind] = !state.modelGroups[kind];
      renderModels();
    });
    cell.append(head);
    row.append(cell);
    return row;
  }
  // GROUP_ORDER 分组渲染次序：普通对话模型在最前（主体），特殊模型分列其后
  // ——用户要求档位/图像等不与普通模型混在一起。
  const GROUP_ORDER = ["chat", "router", "image", "video", "completion"];
  function renderModels() {
    const all = state.models.items;
    const visible = visibleModels();
    const body = $("models-body");
    clearChildren(body);
    $("models-table-wrap").hidden = visible.length === 0;
    $("models-empty").hidden = visible.length !== 0;
    if (!visible.length) {
      const title = $("models-empty").querySelector("strong");
      const detail = $("models-empty").querySelector("p");
      if (title) title.textContent = all.length ? "没有匹配模型" : "暂无模型数据";
      if (detail) detail.textContent = all.length ? "调整搜索或分类筛选后重试。" : "当前账号没有可展示的动态模型。";
      renderModelDetails(null, visible);
      return;
    }
    const groups = groupModels(visible);
    const present = GROUP_ORDER.filter((kind) => groups[kind].length > 0);
    // 单组（只有普通对话模型）时不显示组头——避免给最常见的场景加无意义的标题。
    const grouped = present.length > 1 || (present.length === 1 && present[0] !== "chat");
    // 默认选中项必须落在**可见**行上：折叠组里的模型虽在 visible 里，但没渲染出来，
    // 选中它会让右侧详情面板显示一个列表上看不到的模型（容易让人以为选错了）。
    // 故优先在展开的组内选，全部折叠时回落到第一个可见行。
    const expandedGroups = present.filter((kind) => !grouped || Boolean(state.modelGroups[kind]));
    let selected = visible.find((model) => model.id === state.selectedModelID);
    const selectedVisible = selected && (!grouped || !expandedGroups.length || expandedGroups.includes(modelKindOf(selected)));
    if (!selectedVisible) {
      const candidates = grouped && expandedGroups.length
        ? visible.filter((model) => expandedGroups.includes(modelKindOf(model)))
        : visible;
      selected = candidates[0] || visible[0];
      state.selectedModelID = selected.id;
    }
    present.forEach((kind) => {
      const items = groups[kind];
      const expanded = !grouped || Boolean(state.modelGroups[kind]);
      if (grouped) body.append(modelGroupRow(kind, items.length));
      if (!expanded) return;
      items.forEach((model) => body.append(modelRow(model, selected.id)));
    });
    renderModelDetails(selected, visible);
  }
  function selectModel(modelID) {
    if (!state.models.items.some((model) => model.id === modelID)) return;
    state.selectedModelID = modelID;
    renderModels();
  }
  function renderModelExample(model) {
    const kind = $("model-example-kind").value || "responses";
    const modelKind = modelKindOf(model);
    // 图像模型必须走 Images API——送到 chat completions 会被上游按 11102/11133 拒绝。
    // 示例按模型分类自动选端点，避免复制到一份注定失败的配置。
    const effective = modelKind === "image" && kind !== "image" ? "image" : kind;
    let endpoint = "/v1/responses";
    let body = `{\n  "model": ${JSON.stringify(model.id)},\n  "input": "Hello"\n}`;
    if (effective === "chat") {
      endpoint = "/v1/chat/completions";
      body = `{\n  "model": ${JSON.stringify(model.id)},\n  "messages": [{"role": "user", "content": "Hello"}]\n}`;
    } else if (effective === "image") {
      // 图生图模型（tags 含 image-to-image）走 /v1/images/edits 且必须带输入图；
      // 其余图像模型走 /v1/images/generations。
      const isEdit = Array.isArray(model.tags) && model.tags.some((t) => String(t).toLowerCase() === "image-to-image");
      endpoint = isEdit ? "/v1/images/edits" : "/v1/images/generations";
      body = isEdit
        ? `{\n  "model": ${JSON.stringify(model.id)},\n  "prompt": "把背景换成雪山",\n  "image": "https://example.com/input.png"\n}`
        : `{\n  "model": ${JSON.stringify(model.id)},\n  "prompt": "一只在屋顶上看星星的猫"\n}`;
    }
    $("model-example").textContent = `curl ${location.origin}${endpoint} \\\n  -H "Authorization: Bearer YOUR_API_KEY" \\\n  -H "Content-Type: application/json" \\\n  -d '${body.replace(/'/g, "'\\''")}'`;
  }
  async function loadModels(uid, force = false) {
    if (!uid || !state.csrf) return;
    if (!force && state.models.uid === uid && state.models.loaded) return;
    const session = state.csrf;
    const epoch = state.sessionEpoch;
    const request = ++state.models.request;
    state.models.uid = uid;
    state.models.loaded = false;
    state.models.loading = true;
    state.models.items = [];
    state.models.realm = "";
    state.selectedModelID = "";
    clearModelView("正在读取模型列表…");
    $("models-refresh").disabled = true;
    try {
      const path = force
        ? `accounts/${encodeURIComponent(uid)}/models/refresh`
        : `accounts/${encodeURIComponent(uid)}/models`;
      const out = await api(path, force ? { method: "POST", body: "{}" } : {});
      if (state.csrf !== session || state.sessionEpoch !== epoch || state.models.request !== request || state.models.uid !== uid) return;
      state.models = {
        ...state.models,
        uid,
        realm: out.realm || "",
        items: Array.isArray(out.models) ? out.models : [],
        source: out.source || "unavailable",
        fetchedAt: out.fetched_at || null,
        expiresAt: out.expires_at || null,
        stale: Boolean(out.stale),
        error: out.error || "",
        loaded: true,
        loading: false,
        request,
      };
      renderModelSource();
      if (state.models.source === "unavailable" && !state.models.items.length) {
        clearModelView(state.models.error || "当前账号的模型列表不可用。");
        renderModelSource();
      } else {
        renderModels();
      }
    } catch (err) {
      if (state.csrf !== session || state.sessionEpoch !== epoch || state.models.request !== request || state.models.uid !== uid) return;
      state.models = { ...state.models, source: "unavailable", realm: "", items: [], stale: false, error: err.message, loaded: true, loading: false };
      clearModelView(err.message || "当前账号的模型列表不可用。");
      renderModelSource();
    } finally {
      if (state.csrf === session && state.sessionEpoch === epoch && state.models.request === request) {
        state.models.loading = false;
        $("models-refresh").disabled = false;
      }
    }
  }
  async function refreshModels() {
    const uid = $("model-account").value;
    if (!uid) return notice("暂无可查询的账号", true);
    await loadModels(uid, true);
    if (state.models.source === "stale" || state.models.stale) notice("刷新失败，展示旧快照", true);
    else if (state.models.source !== "unavailable") notice("模型列表已刷新");
  }
  function renderKeys(data) {
    state.keys.items = Array.isArray(data.keys) ? data.keys : [];
    state.keys.authenticationRequired = Boolean(data.authentication_required);
    state.keys.maxKeys = Number.isFinite(Number(data.max_keys)) ? Number(data.max_keys) : 1000;
    state.keys.loaded = true;
    const authState = $("key-auth-state");
    authState.textContent = state.keys.authenticationRequired ? "API Key 鉴权已开启" : "当前为匿名访问";
    authState.className = `key-auth-state ${state.keys.authenticationRequired ? "enabled" : "anonymous"}`;
    $("key-auth-help").textContent = state.keys.authenticationRequired
      ? "客户端请求需要使用有效的 Authorization: Bearer <API_KEY>。"
      : "当前客户端请求无需 API Key；创建首个密钥后会开启鉴权，废弃全部密钥也不会恢复匿名访问。";
    $("key-limits").textContent = `密钥数量 ${state.keys.items.length} / ${state.keys.maxKeys}`;
    const body = $("keys-body");
    clearChildren(body);
    const empty = state.keys.items.length === 0;
    $("keys-empty").hidden = !empty;
    $("keys-table-wrap").hidden = empty;
    $("key-create-button").disabled = state.keys.items.length >= state.keys.maxKeys;
    state.keys.items.forEach((key) => {
      const row = document.createElement("tr");
      row.append(node("td", key.name || "未命名"));
      const prefix = node("td");
      prefix.append(node("code", key.prefix || "—"));
      row.append(prefix);
      row.append(node("td", key.source === "legacy" ? "原配置" : "管理端创建"));
      const active = key.status === "active";
      const status = node("td");
      status.append(node("span", active ? "有效" : "已废弃", `status-badge ${active ? "" : "disabled"}`));
      row.append(status);
      row.append(node("td", fmtTime(key.created_at)));
      row.append(node("td", key.revoked_at ? fmtTime(key.revoked_at) : "—"));
      const actions = node("td");
      if (active) {
        const revoke = node("button", "废弃", "mini-button revoke-button");
        revoke.type = "button";
        revoke.addEventListener("click", () => openRevokeDialog(key));
        actions.append(revoke);
      }
      row.append(actions);
      body.append(row);
    });
  }
  async function loadKeys(force = false) {
    if (!state.csrf || (!force && state.keys.loaded)) return;
    const session = state.csrf;
    const epoch = state.sessionEpoch;
    const request = ++state.keys.request;
    $("key-auth-state").textContent = "正在读取鉴权状态…";
    $("keys-refresh").disabled = true;
    try {
      const out = await api("keys");
      if (state.csrf !== session || state.sessionEpoch !== epoch || state.keys.request !== request) return;
      renderKeys(out);
    } catch (err) {
      if (state.csrf === session && state.sessionEpoch === epoch && state.keys.request === request) {
        $("key-auth-state").textContent = `读取失败：${err.message}`;
        $("key-auth-state").className = "key-auth-state error";
        $("key-auth-help").textContent = "请稍后刷新列表。";
      }
    } finally {
      if (state.csrf === session && state.sessionEpoch === epoch && state.keys.request === request) $("keys-refresh").disabled = false;
    }
  }
  function clearKeySecret() {
    clearCopyFallback();
    state.keyToRevoke = null;
    const secret = $("key-secret");
    if (secret) secret.value = "";
    const dialog = $("key-secret-dialog");
    if (dialog && dialog.open) dialog.close();
    const revokeDialog = $("key-revoke-dialog");
    if (revokeDialog && revokeDialog.open) revokeDialog.close();
  }
  function showKeySecret(secret) {
    const field = $("key-secret");
    field.value = secret;
    const dialog = $("key-secret-dialog");
    if (!dialog.open) dialog.showModal();
    field.focus();
    field.select();
  }
  async function createKey(event) {
    event.preventDefault();
    if (state.keys.inFlight) return;
    const name = $("key-name").value.trim();
    if (!name) return notice("请填写用途名称", true);
    const session = state.csrf;
    const epoch = state.sessionEpoch;
    state.keys.inFlight = true;
    $("key-create-button").disabled = true;
    try {
      const out = await api("keys", { method: "POST", body: JSON.stringify({ name }) });
      if (state.csrf !== session || state.sessionEpoch !== epoch) return;
      $("key-name").value = "";
      state.keys.request += 1;
      $("keys-refresh").disabled = false;
      if (out.key) {
        state.keys.items = [out.key, ...state.keys.items.filter((key) => key.id !== out.key.id)];
        renderKeys({ keys: state.keys.items, authentication_required: true, max_keys: state.keys.maxKeys });
      }
      if (out.secret) showKeySecret(out.secret);
      else notice("密钥已创建，但服务未返回明文密钥", true);
    } catch (err) {
      if (state.csrf === session && state.sessionEpoch === epoch) notice(err.message, true);
    } finally {
      if (state.csrf === session && state.sessionEpoch === epoch) {
        state.keys.inFlight = false;
        $("key-create-button").disabled = state.keys.items.length >= state.keys.maxKeys;
      }
    }
  }
  function openRevokeDialog(key) {
    state.keyToRevoke = key;
    $("key-revoke-message").textContent = `“${key.name || key.prefix || "此密钥"}”废弃后，新请求会立即返回 401，且无法恢复。确定继续吗？`;
    const dialog = $("key-revoke-dialog");
    dialog.returnValue = "";
    if (!dialog.open) dialog.showModal();
  }
  async function revokeKey() {
    const key = state.keyToRevoke;
    state.keyToRevoke = null;
    if (!key || !state.csrf) return;
    const session = state.csrf;
    const epoch = state.sessionEpoch;
    try {
      await api(`keys/${encodeURIComponent(key.id)}/revoke`, { method: "POST", body: "{}" });
      if (state.csrf !== session || state.sessionEpoch !== epoch) return;
      notice("API Key 已废弃");
      state.keys.request += 1;
      await loadKeys(true);
    } catch (err) {
      if (state.csrf === session && state.sessionEpoch === epoch) notice(err.message, true);
    }
  }
  async function loadStatus(silent = false) {
    try {
      const data = await api("status");
      renderStatus(data);
      if (!silent) notice("状态已更新");
    } catch (err) {
      if (state.csrf) {
        serviceState(false);
        if (!silent) notice(err.message, true);
      }
    }
  }
  // ACCOUNT_ACTION_NOTICE 各账号操作的成功文案。措辞刻意区分两个位：
  // 「临时停用/恢复」是运维意图，「复活」解的是系统自动判定——用户看到
  //「已启用」却仍不可选（自动位没清）时最容易困惑，故这里说清楚。
  const ACCOUNT_ACTION_NOTICE = {
    credits: "积分已刷新",
    manual_disable: "账号已临时停用（签到与保活照常，随时可恢复）",
    manual_enable: "账号已恢复，重新参与选号",
    revive: "已清除系统禁用状态；若账号仍被临时停用，请再点「恢复」",
    disable: "账号已停用",
    enable: "账号已启用",
  };
  async function accountAction(uid, action) {
    try {
      // 手动停用带原因（运维留痕）；其余动作空体即可。
      const body =
        action === "manual_disable"
          ? JSON.stringify({ reason: $("manual-disable-reason").value.trim() })
          : "{}";
      await api(`accounts/${encodeURIComponent(uid)}/${action}`, {
        method: "POST",
        body,
      });
      notice(ACCOUNT_ACTION_NOTICE[action] || "操作已完成");
      await loadStatus(true);
    } catch (err) {
      notice(err.message, true);
    }
  }
  async function batchCredits() {
    const accounts = (state.status && state.status.accounts) || [];
    if (!accounts.length) return notice("账号池为空", true);
    $("batch-credits").disabled = true;
    let failed = 0;
    for (const account of accounts) {
      try {
        await api(`accounts/${encodeURIComponent(account.uid)}/credits`, {
          method: "POST",
          body: "{}",
        });
      } catch (_) {
        failed++;
      }
    }
    $("batch-credits").disabled = false;
    await loadStatus(true);
    notice(
      failed ? `刷新完成，${failed} 个账号查询失败` : "所有账号积分已刷新",
      Boolean(failed),
    );
  }
  async function importAccount() {
    const raw = $("import-json").value.trim();
    if (!raw) return notice("请粘贴或选择账号 JSON", true);
    try {
      JSON.parse(raw);
      const out = await api("accounts/import", { method: "POST", body: raw });
      $("import-json").value = "";
      $("import-file").value = "";
      notice(`已导入账号 ${out.nickname || out.uid}`);
      await loadStatus(true);
    } catch (err) {
      notice(err.message || "JSON 格式无效", true);
    }
  }
  function oauthMessage(message, error = false) {
    $("oauth-status").textContent = message;
    $("oauth-status").className = error ? "oauth-status error" : "oauth-status";
  }
  function oauthButton() {
    $("oauth-idle").hidden = Boolean(state.oauth) || state.oauthNeedsRestore || state.oauthChecking;
    $("oauth-start").disabled = state.oauthChecking;
    $("oauth-start").textContent = state.oauthChecking
      ? "正在读取授权状态…"
      : state.oauthNeedsRestore
        ? "重新检查授权状态"
        : state.oauth
          ? "继续当前授权"
          : "添加授权账号";
  }
  function stopOAuth(flow = state.oauth) {
    if (state.oauth !== flow) return;
    if (flow && flow.timer) clearInterval(flow.timer);
    state.oauth = null;
    $("oauth-link").removeAttribute("href");
    $("oauth-active").hidden = true;
    $("oauth-idle").hidden = false;
    oauthButton();
  }
  function showOAuth(out, restored = false) {
    if (out.status === "completed") {
      stopOAuth();
      if (state.oauthCompletedID !== out.id) {
        state.oauthCompletedID = out.id;
        oauthMessage(`授权完成：${out.nickname || out.uid}。账号已加入列表，可刷新积分。`);
        loadStatus(true);
      }
      return;
    }
    let flow = state.oauth;
    if (!flow || flow.id !== out.id) {
      stopOAuth();
      flow = { id: out.id, timer: null, inFlight: false, nextPoll: 0 };
      state.oauth = flow;
      flow.timer = setInterval(() => pollOAuth(flow), 1000);
    }
    flow.expires = new Date(out.expires_at).getTime();
    flow.status = out.status;
    flow.interval = Math.max(3, out.poll_interval_seconds || 3) * 1000;
    $("oauth-idle").hidden = true;
    $("oauth-active").hidden = false;
    $("oauth-link").hidden = !out.auth_url;
    $("oauth-check").hidden = out.status === "starting";
    if (out.auth_url) $("oauth-link").href = out.auth_url;
    else $("oauth-link").removeAttribute("href");
    oauthMessage(out.status === "starting"
      ? "正在获取官方授权链接，请稍候。刷新页面后也会继续恢复。"
      : restored
        ? "已恢复未完成的授权。可重新打开授权页面，完成后将自动添加账号。"
        : "等待你在官方页面完成授权。完成后请返回这里，控制台会自动检测结果。");
    oauthButton();
    updateOAuthCountdown(flow);
  }
  function updateOAuthCountdown(flow) {
    $("oauth-countdown").textContent =
      `授权链接剩余 ${Math.max(0, Math.ceil((flow.expires - Date.now()) / 1000))} 秒`;
  }
  async function restoreOAuth(focus = false) {
    if (state.oauthChecking || !state.csrf) return;
    const session = state.csrf;
    state.oauthChecking = true;
    oauthButton();
    try {
      const out = await api("oauth/current");
      if (state.csrf !== session) return;
      state.oauthNeedsRestore = false;
      if (out.flow) {
        showOAuth(out.flow, true);
        if (focus && out.flow.status !== "completed") setView("accounts");
      } else {
        oauthMessage(state.oauth ? "当前授权已结束或过期，请点击“添加授权账号”重新开始。" : "");
        stopOAuth();
      }
    } catch (err) {
      if (state.csrf === session) {
        state.oauthNeedsRestore = true;
        oauthMessage(`暂时无法恢复授权状态：${err.message}。请点击“重新检查授权状态”。`, true);
      }
    } finally {
      state.oauthChecking = false;
      oauthButton();
    }
  }
  async function cancelOAuth() {
    const flow = state.oauth;
    if (!flow) return;
    if (flow.inFlight) {
      notice("正在检测授权状态，请稍后再取消", true);
      return;
    }
    try {
      await api(`oauth/${encodeURIComponent(flow.id)}`, {
        method: "DELETE",
        body: "{}",
      });
      stopOAuth(flow);
      oauthMessage("授权已取消。需要添加账号时，可重新开始授权。");
    } catch (err) {
      if (err.status === 404) {
        stopOAuth(flow);
        oauthMessage("授权已结束或过期，可重新开始。");
      } else if (state.oauth === flow) oauthMessage(err.message, true);
    }
  }
  async function pollOAuth(flow = state.oauth) {
    if (!flow || state.oauth !== flow || flow.inFlight) return;
    updateOAuthCountdown(flow);
    if (Date.now() >= flow.expires) {
      stopOAuth(flow);
      oauthMessage("授权链接已过期。请点击“添加授权账号”获取新链接，旧页面无需继续操作。", true);
      return;
    }
    if (Date.now() < flow.nextPoll) return;
    flow.nextPoll = Date.now() + flow.interval;
    if (flow.status === "starting") {
      await restoreOAuth();
      return;
    }
    flow.inFlight = true;
    $("oauth-cancel").disabled = true;
    $("oauth-check").disabled = true;
    try {
      const out = await api(`oauth/${encodeURIComponent(flow.id)}/poll`, {
        method: "POST",
        body: "{}",
      });
      if (state.oauth !== flow) return;
      if (out.status === "completed") {
        stopOAuth(flow);
        state.oauthCompletedID = flow.id;
        oauthMessage(`授权完成：${out.nickname || out.uid}。账号已加入列表，可刷新积分。`);
        await loadStatus(true);
      } else {
        oauthMessage("正在等待官方授权确认，控制台会自动检测。也可点击“我已完成授权，立即检测”。");
      }
    } catch (err) {
      if (state.oauth === flow) {
        if (err.status === 404) {
          stopOAuth(flow);
          oauthMessage("授权已过期或已在其他页面取消，请重新开始。", true);
        } else {
          oauthMessage(`授权状态暂时无法确认：${err.message}；将自动重试。`, true);
        }
      }
    } finally {
      flow.inFlight = false;
      $("oauth-cancel").disabled = false;
      $("oauth-check").disabled = false;
    }
  }
  async function startOAuth() {
    if (state.oauthChecking) return;
    if (state.oauthNeedsRestore) return restoreOAuth(true);
    if (state.oauth) {
      $("oauth-panel").scrollIntoView({ behavior: "smooth", block: "center" });
      oauthMessage("已有授权正在进行，请使用下方链接继续；如需换账号，请先取消当前授权。");
      return;
    }
    state.oauthChecking = true;
    oauthButton();
    oauthMessage("正在获取官方授权链接…");
    const session = state.csrf;
    try {
      const out = await api("oauth/start", { method: "POST", body: "{}" });
      if (state.csrf !== session) return;
      showOAuth(out);
      $("oauth-panel").scrollIntoView({ behavior: "smooth", block: "center" });
    } catch (err) {
      if (state.csrf === session) {
        state.oauthNeedsRestore = true;
        oauthMessage(`暂时无法确认授权是否创建：${err.message}。请点击“重新检查授权状态”继续。`, true);
      }
    } finally {
      state.oauthChecking = false;
      oauthButton();
    }
  }
  function deepGet(obj, key) {
    return key.split(".").reduce((value, part) => value && value[part], obj);
  }
  function deepSet(obj, key, value) {
    const parts = key.split(".");
    let cur = obj;
    parts.slice(0, -1).forEach((part) => {
      cur[part] = cur[part] || {};
      cur = cur[part];
    });
    cur[parts.at(-1)] = value;
  }
  function deepMerge(target, source) {
    Object.entries(source).forEach(([key, value]) => {
      if (["__proto__", "constructor", "prototype"].includes(key)) {
        throw new Error("不支持的配置字段");
      }
      if (
        value &&
        !Array.isArray(value) &&
        typeof value === "object" &&
        target[key] &&
        !Array.isArray(target[key]) &&
        typeof target[key] === "object"
      ) {
        deepMerge(target[key], value);
      } else {
        target[key] = value;
      }
    });
    return target;
  }
  function parseHours(value) {
    const text = String(value).trim();
    if (!text) return [];
    const parts = text.split(",").map((part) => part.trim());
    if (parts.some((part) => !/^\d{1,2}$/.test(part))) {
      throw new Error("定时任务小时必须是 0–23 的整数，并用逗号分隔");
    }
    const hours = parts.map(Number);
    if (
      hours.some((hour) => !Number.isInteger(hour) || hour < 0 || hour > 23)
    ) {
      throw new Error("定时任务小时必须是 0–23 的整数，并用逗号分隔");
    }
    return [...new Set(hours)];
  }
  function fillConfig(data) {
    state.config = data;
    document.querySelectorAll("[data-config]").forEach((input) => {
      const value = deepGet(data.config || {}, input.dataset.config);
      if (input.type === "checkbox") input.checked = Boolean(value);
      else if (input.dataset.hours === "true")
        input.value = Array.isArray(value) ? value.join(", ") : "";
      else input.value = value ?? "";
    });
    $("config-storage").textContent = data.storage || "—";
    $("key-state").textContent = data.api_key_managed
      ? data.api_key_configured
        ? "API Key 已独立管理 · 有效密钥已配置"
        : data.api_key_required
          ? "API Key 已独立管理 · 当前无有效密钥，API 请求会被拒绝"
          : "API Key 已独立管理 · 当前为匿名访问"
      : data.api_key_configured
        ? "已配置（不可读取）"
        : "未配置";
    $("restart-state").textContent = data.restart_supported
      ? "可从控制台触发"
      : "需在宿主机执行";
    $("env-overrides").textContent =
      (data.environment_overrides || []).join("、") || "无";
    $("restart-warning").hidden = !data.pending_restart;
    $("restart-button").hidden = !data.restart_supported;
    $("current-config").textContent = JSON.stringify(
      data.config || {},
      null,
      2,
    );
    state.dirtyConfig = false;
  }
  async function loadConfig() {
    try {
      fillConfig(await api("config"));
    } catch (err) {
      notice(err.message, true);
    }
  }
  async function saveConfig(event) {
    event.preventDefault();
    const patch = {};
    try {
      document.querySelectorAll("[data-config]").forEach((input) => {
        let value =
          input.type === "checkbox" ? input.checked : input.value.trim();
        if (input.dataset.hours === "true") value = parseHours(value);
        if (value === "" && input.dataset.config !== "upstream.user_agent") return;
        if (input.type === "number") value = Number(value);
        deepSet(patch, input.dataset.config, value);
      });
    } catch (err) {
      notice(err.message, true);
      return;
    }
    const advanced = $("advanced-json").value.trim();
    if (advanced) {
      try {
        const extra = JSON.parse(advanced);
        if (!extra || Array.isArray(extra) || typeof extra !== "object")
          throw new Error();
        deepMerge(patch, extra);
      } catch (_) {
        return notice("高级 JSON 必须是有效对象", true);
      }
    }
    try {
      $("config-status").textContent = "正在保存…";
      fillConfig(
        await api("config", { method: "PUT", body: JSON.stringify(patch) }),
      );
      $("advanced-json").value = "";
      $("config-status").textContent = "已保存";
      notice("配置已保存。若显示待重启，请在确认后重启服务。");
    } catch (err) {
      $("config-status").textContent = "";
      notice(err.message, true);
    }
  }
  async function restart() {
    try {
      await api("restart", { method: "POST", body: "{}" });
      notice("服务正在重启，连接恢复后请重新登录。");
      showLogin();
    } catch (err) {
      notice(err.message, true);
    }
  }
  async function restoreSession() {
    setLoading(true);
    try {
      const session = await api("session");
      state.csrf = session.csrf_token;
      state.view = views.includes(location.hash.slice(1)) ? location.hash.slice(1) : state.view;
      $("session-label").textContent = `会话至 ${fmtTime(session.expires_at)}`;
      showApp();
      await Promise.all([loadStatus(true), loadConfig(), restoreOAuth(true)]);
      clearInterval(state.timer);
      state.timer = setInterval(() => {
        if (!state.dirtyConfig) loadStatus(true);
        if (!state.oauth && state.oauthNeedsRestore) restoreOAuth();
      }, 20000);
    } catch (_) {
      showLogin();
    } finally {
      setLoading(false);
    }
  }
  document
    .querySelectorAll(".nav-item")
    .forEach((el) =>
      el.addEventListener("click", () => setView(el.dataset.view)),
    );
  document
    .querySelectorAll("[data-go]")
    .forEach((el) =>
      el.addEventListener("click", () => setView(el.dataset.go)),
    );
  $("menu-toggle").addEventListener("click", () =>
    document.querySelector(".sidebar").classList.toggle("open"),
  );
  $("login-form").addEventListener("submit", async (event) => {
    event.preventDefault();
    try {
      const out = await fetch("/admin/api/login", {
        method: "POST",
        credentials: "same-origin",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ password: $("password").value }),
      });
      const data = await out.json();
      if (!out.ok)
        throw new Error((data.error && data.error.message) || "登录失败");
      state.csrf = data.csrf_token;
      $("password").value = "";
      await restoreSession();
    } catch (err) {
      notice(err.message, true);
    }
  });
  $("logout-button").addEventListener("click", async () => {
    try {
      await api("logout", { method: "POST", body: "{}" });
    } catch (_) {}
    showLogin("已退出登录");
  });
  $("refresh-button").addEventListener("click", () => loadStatus());
  $("models-refresh").addEventListener("click", refreshModels);
  $("model-account").addEventListener("change", (event) => {
    state.models.request += 1;
    state.models.uid = "";
    state.models.loaded = false;
    state.selectedModelID = "";
    loadModels(event.target.value, false);
  });
  $("model-search").addEventListener("input", (event) => {
    state.modelSearch = event.target.value;
    if (state.models.items.length) renderModels();
  });
  $("model-kind").addEventListener("change", (event) => {
    state.modelKind = event.target.value || "all";
    // 切到某一分类时自动展开该组（否则用户选了"自动路由档位"却看到折叠的组头，
    // 会以为筛选没生效）。
    if (state.modelKind !== "all") state.modelGroups[state.modelKind] = true;
    if (state.models.items.length) renderModels();
  });
  $("model-sort").addEventListener("change", (event) => {
    state.modelSort = event.target.value;
    if (state.models.items.length) renderModels();
  });
  $("model-picker").addEventListener("change", (event) => selectModel(event.target.value));
  $("model-example-kind").addEventListener("change", () => {
    const model = state.models.items.find((item) => item.id === state.selectedModelID);
    if (model) renderModelExample(model);
  });  $("copy-model-id").addEventListener("click", () => {
    const model = state.models.items.find((item) => item.id === state.selectedModelID);
    if (model) copyText(model.id, "模型名");
  });
  $("copy-model-example").addEventListener("click", () => copyText($("model-example").textContent, "请求示例"));
  $("keys-refresh").addEventListener("click", () => loadKeys(true));
  $("key-create-form").addEventListener("submit", createKey);
  $("key-secret-copy").addEventListener("click", () => copyText($("key-secret").value, "API Key", $("key-secret")));
  $("key-secret-dialog").addEventListener("close", () => {
    $("key-secret").value = "";
    clearCopyFallback();
  });
  $("key-revoke-dialog").addEventListener("close", () => {
    if ($("key-revoke-dialog").returnValue === "confirm") revokeKey();
    else state.keyToRevoke = null;
  });
  $("batch-credits").addEventListener("click", batchCredits);
  $("import-button").addEventListener("click", importAccount);
  $("oauth-start").addEventListener("click", startOAuth);
  $("oauth-cancel").addEventListener("click", cancelOAuth);
  $("oauth-check").addEventListener("click", () => {
    if (state.oauth) {
      state.oauth.nextPoll = 0;
      pollOAuth();
    }
  });
  $("config-reload").addEventListener("click", loadConfig);
  $("config-form").addEventListener("submit", saveConfig);
  $("restart-button").addEventListener("click", () => {
    const dialog = $("restart-dialog");
    if (!dialog.open) {
      dialog.returnValue = "";
      dialog.showModal();
    }
  });
  $("restart-dialog").addEventListener("close", () => {
    if ($("restart-dialog").returnValue === "confirm") restart();
  });
  $("import-file").addEventListener("change", (event) => {
    const file = event.target.files && event.target.files[0];
    if (!file) return;
    const reader = new FileReader();
    reader.onload = () => {
      $("import-json").value = String(reader.result || "");
    };
    reader.onerror = () => notice("无法读取所选文件", true);
    reader.readAsText(file);
  });
  document
    .querySelectorAll("#config-form input, #advanced-json")
    .forEach((el) =>
      el.addEventListener("input", () => {
        state.dirtyConfig = true;
      }),
    );
  restoreSession();
})();
