// Single-page admin panel — zero external dependencies, offline-first.
(function () {
  "use strict";

  // ---- XSS-safe helpers ----

  function textNode(txt) {
    return document.createTextNode(String(txt));
  }

  // ---- Toast notifications (bottom-right, auto-dismiss 4s) ----

  function toast(msg, color) {
    color = color || "#34d399";
    var box = document.createElement("div");
    box.className = "toast toast-ok";
    if (color === "#f87171") box.className = "toast toast-err";
    box.textContent = msg;
    document.getElementById("toast-container").appendChild(box);
    setTimeout(function () { box.remove(); }, 4000);
  }

  // ---- API helpers ----

  // X-Devin2API marks requests as coming from this panel; the server rejects
  // state-changing admin calls without it (CSRF guard).

  function apiPost(path, body) {
    body = body || "";
    return fetch(path, { method: "POST", headers: { "Content-Type": "application/json", "X-Devin2API": "1" }, body: body })
      .then(checkStatus)
      .then(parseJSON);
  }

  function apiGet(path) {
    return fetch(path).then(checkStatus).then(parseJSON);
  }

  function apiDelete(path) {
    return fetch(path, { method: "DELETE", headers: { "X-Devin2API": "1" } }).then(checkStatus);
  }

  function apiPatch(path, body) {
    return fetch(path, { method: "PATCH", headers: { "Content-Type": "application/json", "X-Devin2API": "1" }, body: body })
      .then(checkStatus)
      .then(parseJSON);
  }

  function parseJSON(r) {
    return r.json();
  }

  function checkStatus(r) {
    if (r.ok) return r;
    if (r.status === 401) showAdminLogin(); // session missing or expired
    return r.json().catch(function () { return { error: "HTTP " + r.status }; }).then(function (body) {
      var e = new Error(body.error || "request failed");
      e.status = r.status;
      throw e;
    });
  }

  // ---- Admin login (only when the server has an admin password) ----

  function showAdminLogin() {
    document.getElementById("login-overlay").classList.remove("hidden");
    document.getElementById("admin-password").focus();
  }

  function setupAdminLogin(onReady) {
    var form = document.getElementById("admin-login-form");
    var status = document.getElementById("admin-login-status");
    form.onsubmit = function (ev) {
      ev.preventDefault();
      var pw = document.getElementById("admin-password").value;
      apiPost("/admin/api/login", JSON.stringify({ password: pw })).then(function () {
        document.getElementById("admin-password").value = "";
        status.textContent = "";
        document.getElementById("login-overlay").classList.add("hidden");
        document.getElementById("btn-logout").classList.remove("hidden");
        onReady();
      }).catch(function (e) { status.textContent = e.message; });
    };
    document.getElementById("btn-logout").onclick = function () {
      apiPost("/admin/api/logout", "").then(function () { location.reload(); });
    };
    return apiGet("/admin/api/session").then(function (s) {
      if (s.auth_required) document.getElementById("btn-logout").classList.remove("hidden");
      if (s.auth_required && !s.authenticated) {
        showAdminLogin();
        return false;
      }
      return true;
    });
  }

  // ---- Theme ----

  // The page follows the OS theme until the user picks one; the choice is a
  // per-browser convenience, so storage failures are ignored.
  function applySavedTheme() {
    try {
      var t = localStorage.getItem("devin2api-theme");
      if (t === "light" || t === "dark") document.documentElement.setAttribute("data-theme", t);
    } catch (e) { /* storage unavailable */ }
  }

  function toggleTheme() {
    var root = document.documentElement;
    var cur = root.getAttribute("data-theme");
    if (!cur) cur = window.matchMedia("(prefers-color-scheme: dark)").matches ? "dark" : "light";
    var next = cur === "dark" ? "light" : "dark";
    root.setAttribute("data-theme", next);
    try { localStorage.setItem("devin2api-theme", next); } catch (e) { /* storage unavailable */ }
  }

  // emptyRow fills a table body with one centered message row.
  function emptyRow(tbody, cols, msg) {
    var tr = document.createElement("tr");
    var td = document.createElement("td");
    td.colSpan = cols;
    td.className = "empty";
    td.appendChild(textNode(msg));
    tr.appendChild(td);
    tbody.appendChild(tr);
  }

  // ---- Stats grid ----

  function loadStats() {
    apiGet("/admin/api/stats").then(function (d) {
      var g = document.getElementById("stats-grid");
      while (g.firstChild) g.removeChild(g.firstChild);
      var since = d.since ? "since " + new Date(d.since).toLocaleDateString() : "";
      var items = [
        { label: "Accounts", val: d.accounts, sub: (d.enabled != null ? (d.enabled + "/" + d.accounts + " enabled") : "") },
        { label: "Models", val: (d.models ? d.models : "—"), sub: "" },
        { label: "Requests", val: (d.requests != null ? d.requests : 0), sub: since },
        { label: "Failed", val: (d.failed != null ? d.failed : 0), sub: since }
      ];
      items.forEach(function (it) {
        var c = document.createElement("div");
        c.className = "stat-card";
        [["stat-val", String(it.val)], ["stat-label", it.label], ["stat-sub", it.sub]].forEach(function (p) {
          if (!p[1]) return;
          var span = document.createElement("span");
          span.className = p[0];
          span.appendChild(textNode(p[1]));
          c.appendChild(span);
        });
        g.appendChild(c);
      });
    }).catch(function (e) { console.warn("loadStats:", e.message); });
  }

  // ---- Tab navigation ----

  // Tabs and panels are paired by name: <button data-tab="x"> shows
  // <section id="tab-x">.
  var tabLoaders = {
    accounts: function () { loadAccounts(); },
    tokens: function () { loadTokens(); },
    logs: function () { loadLogs(); },
    settings: function () { loadSettings(); }
  };

  function setupTabs() {
    var tabs = document.querySelectorAll(".tab");
    var panels = document.querySelectorAll(".tab-panel");
    for (var i = 0; i < tabs.length; i++) {
      tabs[i].onclick = (function (tab) {
        return function () {
          var name = tab.getAttribute("data-tab");
          for (var j = 0; j < tabs.length; j++) tabs[j].className = "tab";
          for (var k = 0; k < panels.length; k++) {
            panels[k].className = panels[k].id === "tab-" + name ? "tab-panel active" : "tab-panel";
          }
          tab.className = "tab active";
          if (tabLoaders[name]) tabLoaders[name]();
        };
      })(tabs[i]);
    }
  }

  // ---- Logs tab ----

  function fmtMs(ms) {
    if (ms == null) return "—";
    return ms < 1000 ? ms + " ms" : (ms / 1000).toFixed(1) + " s";
  }

  function loadLogs() {
    apiGet("/admin/api/logs?limit=200").then(function (entries) {
      var tbody = document.querySelector("#log-table tbody");
      while (tbody.firstChild) tbody.removeChild(tbody.firstChild);
      if (!entries.length) {
        emptyRow(tbody, 8, "No chat requests yet.");
        return;
      }
      entries.forEach(function (e) {
        var tr = document.createElement("tr");
        var detail = [];
        if (e.stream) detail.push("stream");
        if (e.attempts > 1) detail.push(e.attempts + " attempts");
        if (e.first_output_ms) detail.push("first output " + fmtMs(e.first_output_ms));
        if (e.tool_calls) detail.push(e.tool_calls + " tool call" + (e.tool_calls > 1 ? "s" : ""));
        if (e.finish && e.finish !== "stop") detail.push(e.finish);
        if (e.error) detail.push(e.error);
        var cells = [
          new Date(e.time).toLocaleTimeString(),
          e.endpoint,
          e.model === e.upstream_model ? e.model : e.model + " → " + e.upstream_model,
          e.account_label || e.account || "—",
          String(e.status),
          fmtMs(e.duration_ms),
          (e.input_tokens || 0) + " / " + (e.output_tokens || 0),
          detail.join(" · ")
        ];
        cells.forEach(function (c, i) {
          var td = document.createElement("td");
          td.appendChild(textNode(c));
          if (i === 4) td.className = e.status === 200 ? "status-ok" : "status-err";
          tr.appendChild(td);
        });
        tbody.appendChild(tr);
      });
    }).catch(function (err) { toast(err.message, "#f87171"); });
  }

  // ---- Escape plan-end date helper ----
  function planEndStr(a) {
    if (!a.usage) return "—";
    if (!a.usage.plan_end) return "—";
    // Upstream sends the Unix epoch (or Go's zero time) when there is no end date.
    var d = new Date(a.usage.plan_end);
    if (isNaN(d) || d.getUTCFullYear() < 2000) return "—";
    return d.toLocaleDateString();
  }

  // ---- Accounts tab ----

  function statusInfo(a) {
    if (a.usage_error && a.usage_error !== "") return { label: "Token dead", cls: "badge badge-dead" };
    if (a.expired) return { label: "Expired", cls: "badge badge-expired" };
    if (!a.enabled) return { label: "Disabled", cls: "badge badge-disabled" };
    return { label: "Active", cls: "badge badge-active" };
  }

  // usageBarHTML builds markup from numbers only (coerced with Number), so no
  // server string reaches innerHTML.
  function usageBarHTML(a) {
    if (!a.usage) return "<td>—</td>";
    var daily = Number(a.usage.daily_remaining_percent);
    var weekly = Number(a.usage.weekly_remaining_percent);
    if (isFinite(daily) && isFinite(weekly)) {
      // The bar shows quota left, matching the label; the tighter limit decides.
      var left = Math.max(0, Math.min(Math.min(daily, weekly), 100));
      var barCls = "usage-bar";
      if (left < 10) barCls = "usage-bar critical";
      else if (left < 30) barCls = "usage-bar high";
      return "<td><div class='usage-bar-track'><div class='" + barCls + "' style='width:" + left + "%'></div></div><span>Daily " + daily + "% · weekly " + weekly + "% left</span></td>";
    }
    return "<td>—</td>";
  }

  // identityCell returns plain text; callers insert it as a text node.
  function identityCell(a) {
    return a.email || a.user_name || a.id;
  }

  var acctCache = [];
  var selected = {}; // account id -> true, kept across reloads

  function statusKey(a) {
    if (a.usage_error) return "dead";
    if (a.expired) return "expired";
    if (!a.enabled) return "disabled";
    return "active";
  }

  // visibleAccounts applies the search box and status filter.
  function visibleAccounts() {
    var q = document.getElementById("acct-search").value.trim().toLowerCase();
    var st = document.getElementById("acct-filter").value;
    return acctCache.filter(function (a) {
      if (st && statusKey(a) !== st) return false;
      if (!q) return true;
      return [a.email, a.user_name, a.nickname, a.id].some(function (v) {
        return v && String(v).toLowerCase().indexOf(q) !== -1;
      });
    });
  }

  function selectedIDs() {
    return acctCache.filter(function (a) { return selected[a.id]; }).map(function (a) { return a.id; });
  }

  function renderAccounts(accs) {
    acctCache = accs;
    // Forget selections of accounts that no longer exist.
    var live = {};
    accs.forEach(function (a) { if (selected[a.id]) live[a.id] = true; });
    selected = live;
    drawAccounts();
  }

  function drawAccounts() {
    var accs = visibleAccounts();
    var tbody = document.querySelector("#acct-table tbody");
    tbody.innerHTML = "";
    if (!acctCache.length) emptyRow(tbody, 8, "No accounts yet. Use \"Add account\" to log in or import one.");
    else if (!accs.length) emptyRow(tbody, 8, "No accounts match the search or filter.");
    for (var i = 0; i < accs.length; i++) {
      var a = accs[i];
      var tr = document.createElement("tr");
      if (selected[a.id]) tr.className = "selected";

      // Selection checkbox
      var tdChk = document.createElement("td");
      tdChk.className = "col-check";
      var chk = document.createElement("input");
      chk.type = "checkbox";
      chk.checked = !!selected[a.id];
      chk.onchange = (function (id, row) {
        return function (ev) {
          if (ev.target.checked) selected[id] = true; else delete selected[id];
          row.className = ev.target.checked ? "selected" : "";
          updateBulkBar();
        };
      })(a.id, tr);
      tdChk.appendChild(chk);
      tr.appendChild(tdChk);

      // Identity: initial avatar, name, and the account id underneath.
      var tdId = document.createElement("td");
      var ident = document.createElement("div");
      ident.className = "ident";
      var avatar = document.createElement("span");
      avatar.className = "avatar";
      avatar.appendChild(textNode(identityCell(a).charAt(0) || "?"));
      var identText = document.createElement("div");
      identText.className = "ident-text";
      var main = document.createElement("span");
      main.className = "ident-main";
      main.title = identityCell(a);
      main.appendChild(textNode(identityCell(a)));
      var sub = document.createElement("span");
      sub.className = "ident-sub";
      sub.appendChild(textNode(a.id));
      identText.appendChild(main);
      identText.appendChild(sub);
      ident.appendChild(avatar);
      ident.appendChild(identText);
      tdId.appendChild(ident);
      tr.appendChild(tdId);

      // Nickname
      var tdNick = document.createElement("td");
      tdNick.appendChild(textNode(a.nickname || "—"));
      tr.appendChild(tdNick);

      // Plan
      var tdPlan = document.createElement("td");
      var planBadge = document.createElement("span");
      planBadge.className = "badge badge-plan";
      planBadge.appendChild(textNode(a.usage ? a.usage.plan : "Free"));
      tdPlan.appendChild(planBadge);
      tr.appendChild(tdPlan);

      // Status
      var tdSt = document.createElement("td");
      var si = statusInfo(a);
      var stBadge = document.createElement("span");
      stBadge.className = si.cls;
      stBadge.appendChild(textNode(si.label));
      tdSt.appendChild(stBadge);
      tr.appendChild(tdSt);

      // Usage bar
      var tmp = document.createElement("template");
      tmp.innerHTML = usageBarHTML(a);
      tr.appendChild(tmp.content.firstChild);

      // Plan end
      var tdPE = document.createElement("td");
      tdPE.appendChild(textNode(planEndStr(a)));
      tr.appendChild(tdPE);

      // Actions
      var tdAct = document.createElement("td");
      var refreshBtn = document.createElement("button");
      refreshBtn.className = "btn-sm btn-refresh";
      refreshBtn.textContent = "Refresh";
      refreshBtn.onclick = (function (id) {
        return function () {
          apiPost("/admin/api/accounts/" + id + "/usage/refresh")
            .then(loadAccounts)
            .catch(function (e) { toast(e.message, "#f87171"); });
        };
      })(a.id);
      tdAct.appendChild(refreshBtn);

      // Enable/disable toggle
      var toggleBtn = document.createElement("button");
      toggleBtn.className = "btn-sm";
      toggleBtn.textContent = a.enabled ? "Disable" : "Enable";
      toggleBtn.onclick = (function (id, cur) {
        return function () {
          var newVal = !cur;
          apiPatch("/admin/api/accounts/" + id, JSON.stringify({ enabled: newVal }))
            .then(function () { loadAccounts(); loadStats(); })
            .catch(function (e) { toast(e.message, "#f87171"); });
        };
      })(a.id, a.enabled);
      tdAct.appendChild(toggleBtn);

      // Delete
      var delBtn = document.createElement("button");
      delBtn.className = "btn-sm btn-del";
      delBtn.textContent = "Delete";
      delBtn.onclick = (function (id, label) {
        return function () {
          if (!confirm("Delete account " + label + "?")) return;
          apiDelete("/admin/api/accounts/" + id)
            .then(function () { loadAccounts(); loadStats(); })
            .catch(function (e) { toast(e.message, "#f87171"); });
        };
      })(a.id, a.nickname || identityCell(a));
      tdAct.appendChild(delBtn);
      tr.appendChild(tdAct);

      tbody.appendChild(tr);
    }

    var count = document.getElementById("acct-count");
    count.textContent = accs.length === acctCache.length
      ? acctCache.length + " account" + (acctCache.length === 1 ? "" : "s")
      : accs.length + " of " + acctCache.length + " shown";
    updateBulkBar();
  }

  // updateBulkBar shows the bulk actions while anything is selected and
  // keeps the header checkbox in step with the visible rows.
  function updateBulkBar() {
    var ids = selectedIDs();
    var bar = document.getElementById("bulk-bar");
    bar.classList.toggle("hidden", ids.length === 0);
    document.getElementById("bulk-count").textContent = ids.length + " selected";
    var vis = visibleAccounts();
    var nSel = vis.filter(function (a) { return selected[a.id]; }).length;
    var all = document.getElementById("acct-check-all");
    all.checked = vis.length > 0 && nSel === vis.length;
    all.indeterminate = nSel > 0 && nSel < vis.length;
  }

  function loadAccounts() {
    apiGet("/admin/api/accounts").then(renderAccounts).catch(function (e) { toast(e.message, "#f87171"); });
  }

  // runPool calls fn(id) for every id with at most limit in flight, reports
  // progress, and resolves with the number of failures.
  function runPool(ids, limit, fn, onProgress) {
    return new Promise(function (resolve) {
      var next = 0, done = 0, failed = 0;
      if (!ids.length) return resolve(0);
      function launch() {
        if (next >= ids.length) return;
        var id = ids[next++];
        fn(id).catch(function () { failed++; }).then(function () {
          done++;
          if (onProgress) onProgress(done, ids.length);
          if (done === ids.length) resolve(failed);
          else launch();
        });
      }
      for (var i = 0; i < Math.min(limit, ids.length); i++) launch();
    });
  }

  // bulkRun runs one action over ids, showing progress on the button, then
  // reloads the lists.
  function bulkRun(btn, verb, ids, fn) {
    if (!ids.length) return;
    var label = btn.innerHTML;
    btn.disabled = true;
    return runPool(ids, 4, fn, function (d, n) { btn.textContent = verb + " " + d + "/" + n + "…"; })
      .then(function (failed) {
        btn.innerHTML = label;
        btn.disabled = false;
        loadAccounts();
        loadStats();
        if (failed) toast(verb + ": " + failed + " of " + ids.length + " failed", "#f87171");
        else toast(verb + ": " + ids.length + " account" + (ids.length === 1 ? "" : "s") + " done", "#34d399");
      });
  }

  function refreshUsage(id) { return apiPost("/admin/api/accounts/" + id + "/usage/refresh"); }

  function setEnabled(on) {
    return function (id) { return apiPatch("/admin/api/accounts/" + id, JSON.stringify({ enabled: on })); };
  }

  function refreshAllUsage() {
    var ids = acctCache.filter(function (a) { return a.tokens && a.tokens.session_token; })
      .map(function (a) { return a.id; });
    if (ids.length === 0) return toast("No accounts with tokens to refresh", "#f87171");
    bulkRun(document.getElementById("btn-refresh-all"), "Refreshing", ids, refreshUsage);
  }

  function setupBulk() {
    var search = document.getElementById("acct-search");
    search.oninput = drawAccounts;
    document.getElementById("acct-filter").onchange = drawAccounts;
    document.getElementById("acct-check-all").onchange = function (ev) {
      visibleAccounts().forEach(function (a) {
        if (ev.target.checked) selected[a.id] = true; else delete selected[a.id];
      });
      drawAccounts();
    };
    document.getElementById("bulk-clear").onclick = function () { selected = {}; drawAccounts(); };
    document.getElementById("bulk-refresh").onclick = function () {
      bulkRun(this, "Refreshing", selectedIDs(), refreshUsage);
    };
    document.getElementById("bulk-enable").onclick = function () {
      bulkRun(this, "Enabling", selectedIDs(), setEnabled(true));
    };
    document.getElementById("bulk-disable").onclick = function () {
      bulkRun(this, "Disabling", selectedIDs(), setEnabled(false));
    };
    document.getElementById("bulk-delete").onclick = function () {
      var ids = selectedIDs();
      if (!ids.length || !confirm("Delete " + ids.length + " account" + (ids.length === 1 ? "" : "s") + "? This cannot be undone.")) return;
      var btn = this;
      bulkRun(btn, "Deleting", ids, function (id) {
        return apiDelete("/admin/api/accounts/" + id).then(function () { delete selected[id]; });
      });
    };
  }

  // ---- Tokens (API Keys) tab ----

  function renderTokens(toks) {
    var tbody = document.querySelector("#tok-table tbody");
    tbody.innerHTML = "";
    if (!toks.length) emptyRow(tbody, 4, "No API keys. Without keys, only local clients may call /v1.");
    for (var i = 0; i < toks.length; i++) {
      var t = toks[i];
      var tr = document.createElement("tr");
      var tdID = document.createElement("td");
      tdID.appendChild(textNode(t.id));
      tr.appendChild(tdID);
      var tdC = document.createElement("td");
      tdC.appendChild(textNode(t.created_at ? new Date(t.created_at).toLocaleString() : "—"));
      tr.appendChild(tdC);
      var tdLU = document.createElement("td");
      tdLU.appendChild(textNode(t.last_used ? new Date(t.last_used).toLocaleString() : "never"));
      tr.appendChild(tdLU);
      var tdR = document.createElement("td");
      var revBtn = document.createElement("button");
      revBtn.className = "btn-sm btn-del";
      revBtn.textContent = "Revoke";
      revBtn.onclick = (function (id) {
        return function () {
          if (!confirm("Revoke token " + id + "?")) return;
          apiDelete("/admin/api/tokens/" + id)
            .then(loadTokens)
            .catch(function (e) { toast(e.message, "#f87171"); });
        };
      })(t.id);
      tdR.appendChild(revBtn);
      tr.appendChild(tdR);
      tbody.appendChild(tr);
    }
  }

  function loadTokens() {
    apiGet("/admin/api/tokens").then(renderTokens).catch(function (e) { toast(e.message, "#f87171"); });
  }

  // ---- Settings tab ----

  // savedSettings is the server's copy; draft is what the form shows. Save
  // is enabled while they differ.
  var savedSettings = null;
  var draft = null;
  var defaultUpstream = "";

  function editable(s) {
    return {
      upstreams: (s.upstreams || []).slice(),
      system_prompt: s.system_prompt || "",
      system_prompt_enabled: !!s.system_prompt_enabled,
      system_prompt_mode: s.system_prompt_mode || "prepend"
    };
  }

  function isDirty() {
    return JSON.stringify(draft) !== JSON.stringify(savedSettings);
  }

  function markDirty() {
    var dirty = isDirty();
    document.getElementById("settings-dirty").classList.toggle("hidden", !dirty);
    document.getElementById("btn-settings-save").disabled = !dirty;
    document.getElementById("btn-settings-reset").disabled = !dirty;
  }

  function iconButton(icon, title, onclick, cls) {
    var b = document.createElement("button");
    b.className = "btn-icon" + (cls ? " " + cls : "");
    b.title = title;
    b.innerHTML = "<svg class='icon'><use href='#i-" + icon + "'/></svg>";
    b.onclick = onclick;
    return b;
  }

  function renderUpstreams() {
    var list = document.getElementById("upstream-list");
    list.innerHTML = "";
    if (!draft.upstreams.length) {
      var p = document.createElement("div");
      p.className = "upstream-default";
      p.appendChild(textNode("Using the default endpoint "));
      var c = document.createElement("code");
      c.appendChild(textNode(defaultUpstream));
      p.appendChild(c);
      list.appendChild(p);
      return;
    }
    draft.upstreams.forEach(function (u, i) {
      var row = document.createElement("div");
      row.className = "upstream-row";
      var idx = document.createElement("span");
      idx.className = "upstream-idx";
      idx.appendChild(textNode(i === 0 ? "Primary" : "Fallback " + i));
      var input = document.createElement("input");
      input.type = "url";
      input.value = u;
      input.placeholder = "https://server.codeium.com";
      input.oninput = function () { draft.upstreams[i] = input.value; markDirty(); };
      var result = document.createElement("span");
      result.className = "upstream-result";
      var test = document.createElement("button");
      test.className = "btn-sm";
      test.textContent = "Test";
      test.onclick = function () { testUpstream(input.value, result, test); };
      var up = iconButton("up", "Move up", function () { moveUpstream(i, -1); });
      up.disabled = i === 0;
      var down = iconButton("down", "Move down", function () { moveUpstream(i, 1); });
      down.disabled = i === draft.upstreams.length - 1;
      var del = iconButton("trash", "Remove", function () {
        draft.upstreams.splice(i, 1);
        renderUpstreams();
        markDirty();
      }, "danger");
      [idx, input, test, up, down, del, result].forEach(function (el) { row.appendChild(el); });
      list.appendChild(row);
    });
  }

  function moveUpstream(i, d) {
    var j = i + d;
    var u = draft.upstreams;
    if (j < 0 || j >= u.length) return;
    var t = u[i]; u[i] = u[j]; u[j] = t;
    renderUpstreams();
    markDirty();
  }

  function testUpstream(url, out, btn) {
    out.className = "upstream-result";
    out.textContent = "Testing…";
    btn.disabled = true;
    apiPost("/admin/api/settings/upstream-test", JSON.stringify({ url: url })).then(function (r) {
      out.className = "upstream-result " + (r.ok ? "ok" : "err");
      out.textContent = r.ok ? "Reachable · HTTP " + r.status + " · " + r.ms + " ms" : "Unreachable · " + r.error;
    }).catch(function (e) {
      out.className = "upstream-result err";
      out.textContent = e.message;
    }).then(function () { btn.disabled = false; });
  }

  function renderPromptForm() {
    document.getElementById("sp-enabled").checked = draft.system_prompt_enabled;
    document.getElementById("sp-mode").value = draft.system_prompt_mode;
    document.getElementById("sp-text").value = draft.system_prompt;
    updatePromptCount();
  }

  function updatePromptCount() {
    var n = draft.system_prompt.length;
    document.getElementById("sp-count").textContent = n + " characters" + (draft.system_prompt_enabled || !n ? "" : " · not applied while off");
  }

  function renderSettings(s) {
    savedSettings = editable(s);
    draft = editable(s);
    defaultUpstream = s.default_upstream || "";
    var dl = document.getElementById("settings-dl");
    dl.innerHTML = "";
    var fields = [["Listen", s.listen], ["Data dir", s.data_dir], ["Version", s.version]];
    for (var i = 0; i < fields.length; i++) {
      var dt = document.createElement("dt");
      dt.appendChild(textNode(fields[i][0]));
      dl.appendChild(dt);
      var dd = document.createElement("dd");
      dd.appendChild(textNode(fields[i][1] || "—"));
      dl.appendChild(dd);
    }
    renderUpstreams();
    renderPromptForm();
    markDirty();
  }

  function loadSettings() {
    // Reopening the tab must not throw away edits that were not saved.
    if (draft && isDirty()) return;
    apiGet("/admin/api/settings").then(renderSettings).catch(function (e) { toast(e.message, "#f87171"); });
  }

  function saveSettings() {
    var body = editable(draft);
    body.upstreams = body.upstreams.map(function (u) { return u.trim(); }).filter(Boolean);
    var btn = document.getElementById("btn-settings-save");
    btn.disabled = true;
    fetch("/admin/api/settings", {
      method: "PUT",
      headers: { "Content-Type": "application/json", "X-Devin2API": "1" },
      body: JSON.stringify(body)
    }).then(checkStatus).then(parseJSON).then(function (s) {
      renderSettings(s);
      toast("Settings saved", "#34d399");
    }).catch(function (e) {
      markDirty();
      toast(e.message, "#f87171");
    });
  }

  function setupSettings() {
    document.getElementById("btn-upstream-add").onclick = function () {
      // The first added row starts from the default so the order is explicit.
      if (!draft.upstreams.length && defaultUpstream) draft.upstreams.push(defaultUpstream);
      draft.upstreams.push("");
      renderUpstreams();
      markDirty();
      var inputs = document.querySelectorAll("#upstream-list input");
      if (inputs.length) inputs[inputs.length - 1].focus();
    };
    document.getElementById("sp-enabled").onchange = function (ev) {
      draft.system_prompt_enabled = ev.target.checked; updatePromptCount(); markDirty();
    };
    document.getElementById("sp-mode").onchange = function (ev) { draft.system_prompt_mode = ev.target.value; markDirty(); };
    document.getElementById("sp-text").oninput = function (ev) {
      draft.system_prompt = ev.target.value; updatePromptCount(); markDirty();
    };
    document.getElementById("btn-settings-save").onclick = saveSettings;
    document.getElementById("btn-settings-reset").onclick = function () {
      draft = editable(savedSettings);
      renderUpstreams();
      renderPromptForm();
      markDirty();
    };
  }

  // ---- Login flow ----

  var loginPollInterval = null;

  function startLoginFlow() {
    apiPost("/admin/api/login/start").then(function (resp) {
      var link = document.getElementById("login-link");
      link.href = resp.login_url;
      link.textContent = resp.login_url;
      document.getElementById("login-status").textContent = "Waiting for login...";
      document.getElementById("login-area").className = "login-area";
      pollLoginStatus(resp.login_url);
    }).catch(function (e) { toast("Login start failed: " + e.message, "#f87171"); });
  }

  function pollLoginStatus() {
    // A second "Start login" replaces the first poll instead of adding one.
    if (loginPollInterval) clearInterval(loginPollInterval);
    loginPollInterval = setInterval(function () {
      apiGet("/admin/api/login/status").then(function (resp) {
        if (resp.status === "complete") {
          clearInterval(loginPollInterval);
          loginPollInterval = null;
          document.getElementById("login-status").textContent = "Login complete!";
          closeModal();
          loadAccounts();
          loadStats();
          toast("Account added via login", "#34d399");
        } else if (resp.status === "error") {
          clearInterval(loginPollInterval);
          loginPollInterval = null;
          document.getElementById("login-status").textContent = "Error: " + (resp.error || "unknown");
          toast("Login failed: " + (resp.error || ""), "#f87171");
        } else {
          document.getElementById("login-status").textContent = "Waiting for login...";
        }
      }).catch(function () {
        /* keep polling */
      });
    }, 2000);
  }

  function cancelLogin() {
    if (loginPollInterval) {
      clearInterval(loginPollInterval);
      loginPollInterval = null;
    }
    apiPost("/admin/api/login/cancel").then(function () {
      document.getElementById("login-area").className = "login-area hidden";
    }).catch(function () {
      document.getElementById("login-area").className = "login-area hidden";
    });
  }

  // ---- Modal ----

  function openModal() {
    document.getElementById("modal-overlay").className = "modal-overlay";
    document.getElementById("import-status").textContent = "";
    document.getElementById("import-ta").value = "";
    document.getElementById("manual-status").textContent = "";
    document.getElementById("manual-token").value = "";
    document.getElementById("login-area").className = "login-area hidden";
  }

  // Closing the dialog while a browser login is pending cancels it on the
  // server too, so it does not stay pending in the background.
  function closeModal() {
    document.getElementById("modal-overlay").className = "modal-overlay hidden";
    if (loginPollInterval) cancelLogin();
  }

  function addManualToken() {
    var status = document.getElementById("manual-status");
    var tok = document.getElementById("manual-token").value.trim();
    if (!tok) return status.textContent = "paste a token first";
    status.textContent = "Checking token…";
    apiPost("/admin/api/login/manual", JSON.stringify({ token: tok })).then(function () {
      closeModal();
      loadAccounts();
      loadStats();
      toast("Account added", "#34d399");
    }).catch(function (e) { status.textContent = e.message; });
  }

  // ---- API tab (static content) ----

  function renderApiTab() {
    var dl = document.getElementById("api-dl");
    dl.innerHTML = "";
    var entries = [
      ["/healthz", "GET", 'Returns {"status":"ok"}'],
      ["/v1/models", "GET", "List available models (OpenAI-compatible)"],
      ["/v1/chat/completions", "POST", 'Chat completions via Devin (stream + non-stream, tool calls)'],
      ["/v1/messages", "POST", "Anthropic Messages API (Claude Code, Anthropic SDKs)"],
      ["/v1/responses", "POST", "OpenAI Responses API (previous_response_id kept 1 h in memory)"],
      ["/v1/completions", "POST", "Legacy text completions, emulated on the chat models"],
      ["/admin/api/accounts", "GET / POST", "List or import accounts"],
      ["/admin/api/stats", "GET", 'Returns {accounts,enabled,models,requests,failed,since}'],
      ["/admin/api/logs", "GET", "Recent chat requests (metadata only), newest first"]
    ];
    for (var i = 0; i < entries.length; i++) {
      var dt = document.createElement("dt");
      var method = document.createElement("span");
      method.className = entries[i][1].indexOf("POST") === 0 ? "method post" : "method";
      method.appendChild(textNode(entries[i][1]));
      dt.appendChild(method);
      dt.appendChild(textNode(entries[i][0]));
      dl.appendChild(dt);
      var dd = document.createElement("dd");
      dd.appendChild(textNode(entries[i][2]));
      dl.appendChild(dd);
    }
    var base = location.origin;
    var codeEl = document.getElementById("curl-example");
    codeEl.textContent = 'curl ' + base + '/v1/models\n\n'
      + 'curl -X POST ' + base + '/v1/chat/completions \\\n'
      + '  -H "Authorization: Bearer YOUR_KEY" \\\n'
      + '  -H "Content-Type: application/json" \\\n'
      + '  -d \'{"model":"devin","messages":[{"role":"user","content":"hi"}]}\'';
  }

  // ---- Boot ----

  function loadInitialData() {
    loadStats();
    loadAccounts();
  }

  function boot() {
    applySavedTheme();
    document.getElementById("btn-theme").onclick = toggleTheme;
    setupTabs();
    setupBulk();
    setupSettings();
    renderTokens([]); // placeholder until tab clicked
    setupAdminLogin(loadInitialData).then(function (ready) {
      if (ready) loadInitialData();
    }).catch(function (e) { toast(e.message, "#f87171"); });

    // Buttons
    document.getElementById("btn-add-account").onclick = openModal;
    document.getElementById("btn-modal-close").onclick = closeModal;
    document.getElementById("btn-login-start").onclick = startLoginFlow;
    document.getElementById("btn-login-cancel").onclick = cancelLogin;
    document.getElementById("btn-manual-token").onclick = addManualToken;
    document.getElementById("btn-import").onclick = function () {
      var raw = document.getElementById("import-ta").value.trim();
      if (!raw) return document.getElementById("import-status").textContent = "paste JSON first";
      apiPost("/admin/api/accounts", raw).then(function () {
        closeModal();
        loadAccounts();
        loadStats();
        toast("Account imported", "#34d399");
      }).catch(function (e) { document.getElementById("import-status").textContent = e.message; });
    };
    document.getElementById("btn-refresh-all").onclick = refreshAllUsage;
    document.getElementById("btn-create-token").onclick = function () {
      var box = document.getElementById("new-token-box");
      // Build DOM elements (never innerHTML for security).
      var header = document.createElement("strong");
      header.className = "warn-once";
      header.textContent = "⚠ Shown once — copy now!";
      var valDiv = document.createElement("div");
      valDiv.className = "token-value";
      var codeEl = document.createElement("code");
      codeEl.title = "Click to copy";
      codeEl.onclick = function () {
        // tok is scoped to the create callback below; copy what is displayed.
        navigator.clipboard.writeText(codeEl.textContent).then(function () { toast("Copied!", "#34d399"); });
      };
      // Assemble fresh: clear any stale content first.
      while (box.firstChild) { box.removeChild(box.firstChild); }
      box.appendChild(header);
      box.appendChild(valDiv);
      box.appendChild(codeEl);
      // Display the value.
      apiPost("/admin/api/tokens", "").then(function (tok) {
        codeEl.textContent = tok.token;
        box.className = "new-token-box";
        loadTokens(); // list the new key (masked)
      }).catch(function (e) {
        box.textContent = e.message;
        box.className = "new-token-box err";
      });
      box.style.display = "";
    };
    document.getElementById("btn-refresh-logs").onclick = loadLogs;
    renderApiTab();
  }

  boot();
})();
