// Single-page admin panel — zero external dependencies, offline-first.
(function () {
  "use strict";

  // ---- XSS-safe helpers ----

  function esc(text) {
    var d = document.createElement("div");
    d.textContent = String(text);
    return d.innerHTML;
  }

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
    return r.json().catch(function () { return { error: "HTTP " + r.status }; }).then(function (body) {
      var e = new Error(body.error || "request failed");
      e.status = r.status;
      throw e;
    });
  }

  // ---- Stats grid ----

  function loadStats() {
    apiGet("/admin/api/stats").then(function (d) {
      var g = document.getElementById("stats-grid");
      g.innerHTML = "";
      var items = [
        { label: "Accounts", val: d.accounts, sub: (d.enabled != null ? (d.enabled + "/" + d.accounts) : "") },
        { label: "Models", val: (d.models ? d.models : "—"), sub: "" },
        { label: "Requests", val: (d.requests != null ? d.requests : 0), sub: "" },
        { label: "Failed", val: (d.failed != null ? d.failed : 0), sub: "" }
      ];
      for (var i = 0; i < items.length; i++) {
        var c = document.createElement("div");
        c.className = "stat-card";
        c.innerHTML = "<span class=\"stat-val\">";
        if (items[i].sub) {
          c.children[0].textContent = items[i].val;
        } else {
          c.children[0].innerHTML = "";
          c.children[0].appendChild(textNode(items[i].val));
        }
        c.innerHTML += "</span><span class=\"stat-label\">" + esc(items[i].label) + "</span>";
        if (items[i].sub) {
          c.innerHTML += '<span class="stat-sub">' + esc(items[i].sub) + '</span>';
        }
        g.appendChild(c);
      }
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
        var tr = document.createElement("tr");
        var td = document.createElement("td");
        td.colSpan = 8;
        td.className = "note";
        td.appendChild(textNode("No chat requests yet."));
        tr.appendChild(td);
        tbody.appendChild(tr);
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
    return new Date(a.usage.plan_end).toLocaleDateString();
  }

  // ---- Accounts tab ----

  function statusInfo(a) {
    if (a.usage_error && a.usage_error !== "") return { label: "Token dead", cls: "badge badge-dead" };
    if (a.expired) return { label: "Expired", cls: "badge badge-expired" };
    if (!a.enabled) return { label: "Disabled", cls: "badge badge-disabled" };
    return { label: "Active", cls: "badge badge-active" };
  }

  function usageBarHTML(a) {
    if (!a.usage) return "<td>—</td>";
    var daily = a.usage.daily_remaining_percent;
    var weekly = a.usage.weekly_remaining_percent;
    if (daily != null && weekly != null) {
      var usedPct = 100 - daily;
      var barCls = "usage-bar high";
      if (usedPct > 90) barCls = "usage-bar critical";
      else if (usedPct > 70) barCls = "usage-bar high";
      return "<td><div class='usage-bar-track'><div class='" + barCls + "' style='width:" + Math.min(usedPct, 100) + "%'></div></div><span>Daily: " + daily + "% left</span></td>";
    }
    return "<td>—</td>";
  }

  function identityCell(a) {
    if (a.email) return esc(a.email);
    if (a.user_name) return esc(a.user_name);
    return esc(a.id);
  }

  var acctCache = [];

  function renderAccounts(accs) {
    acctCache = accs;
    var tbody = document.querySelector("#acct-table tbody");
    tbody.innerHTML = "";
    for (var i = 0; i < accs.length; i++) {
      var a = accs[i];
      var tr = document.createElement("tr");

      // Identity
      var tdId = document.createElement("td");
      tdId.appendChild(textNode(identityCell(a)));
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
            .then(loadAccounts)
            .catch(function (e) { toast(e.message, "#f87171"); });
        };
      })(a.id, a.enabled);
      tdAct.appendChild(toggleBtn);

      // Delete
      var delBtn = document.createElement("button");
      delBtn.className = "btn-sm btn-del";
      delBtn.textContent = "Delete";
      delBtn.onclick = (function (id) {
        return function () {
          if (!confirm("Delete account " + (a.nickname || a.id) + "?")) return;
          apiDelete("/admin/api/accounts/" + id)
            .then(loadAccounts)
            .catch(function (e) { toast(e.message, "#f87171"); });
        };
      })(a.id);
      tdAct.appendChild(delBtn);

      tbody.appendChild(tr);
    }
  }

  function loadAccounts() {
    apiGet("/admin/api/accounts").then(renderAccounts).catch(function (e) { toast(e.message, "#f87171"); });
  }

  function refreshAllUsage() {
    var ids = [];
    for (var i = 0; i < acctCache.length; i++) {
      if (acctCache[i].tokens && acctCache[i].tokens.session_token) {
        ids.push(acctCache[i].id);
      }
    }
    if (ids.length === 0) return toast("No accounts with tokens to refresh", "#f87171");
    var idx = 0;
    function next() {
      if (idx >= ids.length) return toast("Refresh complete", "#34d399");
      var id = ids[idx++];
      apiPost("/admin/api/accounts/" + id + "/usage/refresh")
        .then(function () { next(); })
        .catch(function () { next(); });
    }
    next();
  }

  // ---- Tokens (API Keys) tab ----

  function renderTokens(toks) {
    var tbody = document.querySelector("#tok-table tbody");
    tbody.innerHTML = "";
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

  function loadSettings() {
    apiGet("/admin/api/settings").then(function (s) {
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
    }).catch(function (e) { toast(e.message, "#f87171"); });
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
    loginPollInterval = setInterval(function () {
      apiGet("/admin/api/login/status").then(function (resp) {
        if (resp.status === "complete") {
          clearInterval(loginPollInterval);
          loginPollInterval = null;
          document.getElementById("login-status").textContent = "Login complete!";
          closeModal();
          loadAccounts();
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
    document.getElementById("login-area").className = "login-area hidden";
  }

  function closeModal() {
    document.getElementById("modal-overlay").className = "modal-overlay hidden";
    if (loginPollInterval) {
      clearInterval(loginPollInterval);
      loginPollInterval = null;
    }
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
      ["/v1/responses", "POST", "OpenAI Responses API (stateless)"],
      ["/v1/messages", "POST", 'Anthropic Messages API (Claude Code, Anthropic SDKs)'],
      ["/v1/responses", "POST", 'OpenAI Responses API (stateless)'],
      ["/admin/api/accounts", "GET / POST", "List or import accounts"],
      ["/admin/api/stats", "GET", 'Returns {accounts,enabled,models,requests,failed}'],
      ["/admin/api/logs", "GET", "Recent chat requests (metadata only), newest first"]
    ];
    for (var i = 0; i < entries.length; i++) {
      var dt = document.createElement("dt");
      dt.appendChild(textNode(entries[i][1] + " " + entries[i][0]));
      dl.appendChild(dt);
      var dd = document.createElement("dd");
      dd.appendChild(textNode(entries[i][2]));
      dl.appendChild(dd);
    }
    var codeEl = document.getElementById("curl-example");
    codeEl.textContent = 'curl http://localhost:8080/v1/models\n'
      + 'curl -X POST http://localhost:8080/v1/chat/completions \\\n'
      + '  -H "Authorization: Bearer YOUR_KEY" \\\n'
      + '  -H "Content-Type: application/json" \\\n'
      + '  -d \'{"model":"devin","messages":[{"role":"user","content":"hi"}]}\'';
  }

  // ---- Boot ----

  function boot() {
    setupTabs();
    loadStats();
    loadAccounts();
    renderTokens([]); // placeholder until tab clicked

    // Buttons
    document.getElementById("btn-add-account").onclick = openModal;
    document.getElementById("btn-modal-close").onclick = closeModal;
    document.getElementById("btn-login-start").onclick = startLoginFlow;
    document.getElementById("btn-login-cancel").onclick = cancelLogin;
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
