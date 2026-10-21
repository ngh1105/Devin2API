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

  function apiPut(path, body) {
    return fetch(path, { method: "PUT", headers: { "Content-Type": "application/json", "X-Devin2API": "1" }, body: body })
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

  // The page follows the OS theme ("system") until the user picks light or
  // dark; the choice is a per-browser convenience, so storage failures are
  // ignored.
  function applyTheme(choice) {
    var root = document.documentElement;
    if (choice === "light" || choice === "dark") root.setAttribute("data-theme", choice);
    else { choice = "system"; root.removeAttribute("data-theme"); }
    var opts = document.querySelectorAll(".theme-opt");
    for (var i = 0; i < opts.length; i++) {
      var on = opts[i].getAttribute("data-theme-choice") === choice;
      opts[i].classList.toggle("active", on);
      opts[i].setAttribute("aria-checked", on ? "true" : "false");
    }
  }

  function setupTheme() {
    var saved = "system";
    try { saved = localStorage.getItem("devin2api-theme") || "system"; } catch (e) { /* storage unavailable */ }
    applyTheme(saved);
    var opts = document.querySelectorAll(".theme-opt");
    for (var i = 0; i < opts.length; i++) {
      opts[i].onclick = function () {
        var choice = this.getAttribute("data-theme-choice");
        applyTheme(choice);
        try { localStorage.setItem("devin2api-theme", choice); } catch (e) { /* storage unavailable */ }
      };
    }
  }

  // setNavCount shows a small count next to a sidebar item, or nothing.
  function setNavCount(id, n) {
    var el = document.getElementById(id);
    if (el) el.textContent = n > 0 ? String(n) : "";
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

  // ---- Dialogs ----

  // The open dialog's close function, so Escape and a backdrop click can
  // dismiss whichever dialog is on top.
  var dialogClose = null;

  // showDialog opens the overlay `id`. opts.onClose runs when it closes;
  // opts.sticky keeps Escape and backdrop clicks from closing it (used for
  // the one-time key reveal, whose value cannot be shown again). Focus is
  // kept inside the dialog and returned to the opener on close.
  function showDialog(id, opts) {
    opts = opts || {};
    if (typeof opts === "function") opts = { onClose: opts };
    var el = document.getElementById(id);
    var opener = document.activeElement;
    el.classList.remove("hidden");
    var close = function () {
      el.classList.add("hidden");
      if (dialogClose === close) dialogClose = null;
      if (opts.onClose) opts.onClose();
      if (opener && opener.focus && document.body.contains(opener)) opener.focus();
    };
    close.sticky = !!opts.sticky;
    close.el = el;
    dialogClose = close;
  }

  // confirmDialog asks a yes/no question in a styled dialog; it resolves
  // true when the action is confirmed and false when it is dismissed.
  function confirmDialog(title, message, action) {
    return new Promise(function (resolve) {
      document.getElementById("confirm-title").textContent = title;
      document.getElementById("confirm-msg").textContent = message;
      var ok = document.getElementById("confirm-ok");
      var cancel = document.getElementById("confirm-cancel");
      ok.textContent = action || "Delete";
      var settled = false;
      var settle = function (v) {
        if (settled) return;
        settled = true;
        ok.onclick = null;
        cancel.onclick = null;
        resolve(v);
      };
      var done = function (v) {
        settle(v);
        if (dialogClose) dialogClose();
      };
      showDialog("confirm-overlay", { onClose: function () { settle(false); } });
      ok.onclick = function () { done(true); };
      cancel.onclick = function () { done(false); };
      // Focus the safe choice: Enter must not delete by accident.
      cancel.focus();
    });
  }

  // focusable lists the elements Tab can reach inside a dialog.
  function focusable(root) {
    var nodes = root.querySelectorAll("button, [href], input, select, textarea, [tabindex]");
    return Array.prototype.filter.call(nodes, function (n) {
      return !n.disabled && n.tabIndex >= 0 && n.offsetParent !== null;
    });
  }

  function setupDialogs() {
    document.addEventListener("keydown", function (ev) {
      if (!dialogClose) return;
      if (ev.key === "Escape") {
        if (!dialogClose.sticky) dialogClose();
        return;
      }
      if (ev.key !== "Tab") return;
      // Trap Tab inside the open dialog.
      var items = focusable(dialogClose.el);
      if (!items.length) return;
      var first = items[0], last = items[items.length - 1];
      if (ev.shiftKey && (document.activeElement === first || !dialogClose.el.contains(document.activeElement))) {
        ev.preventDefault(); last.focus();
      } else if (!ev.shiftKey && (document.activeElement === last || !dialogClose.el.contains(document.activeElement))) {
        ev.preventDefault(); first.focus();
      }
    });
    ["confirm-overlay", "key-overlay", "reveal-overlay", "modal-overlay"].forEach(function (id) {
      var el = document.getElementById(id);
      if (!el) return;
      el.addEventListener("mousedown", function (ev) {
        if (ev.target === el && dialogClose && !dialogClose.sticky) dialogClose();
      });
    });
  }

  // ---- Pager ----

  // renderPager draws "1–25 of 120" with page buttons and a page-size
  // picker; onChange(page, size) redraws the table.
  function renderPager(el, total, page, size, onChange) {
    el.innerHTML = "";
    if (total <= 10) return;
    var pages = Math.max(1, Math.ceil(total / size));
    var info = document.createElement("span");
    info.className = "pager-info";
    var from = page * size + 1, to = Math.min(total, (page + 1) * size);
    info.appendChild(textNode(from + "–" + to + " of " + total));
    var sel = document.createElement("select");
    [10, 25, 50, 100].forEach(function (n) {
      var o = document.createElement("option");
      o.value = n;
      o.textContent = n + " per page";
      if (n === size) o.selected = true;
      sel.appendChild(o);
    });
    sel.onchange = function () { onChange(0, Number(sel.value)); };
    var nav = document.createElement("div");
    nav.className = "pager-nav";
    var prev = iconButton("up", "Previous page", function () { onChange(page - 1, size); });
    prev.classList.add("rot-left");
    prev.disabled = page === 0;
    var label = document.createElement("span");
    label.className = "pager-page";
    label.appendChild(textNode("Page " + (page + 1) + " / " + pages));
    var next = iconButton("down", "Next page", function () { onChange(page + 1, size); });
    next.classList.add("rot-left");
    next.disabled = page >= pages - 1;
    nav.appendChild(prev);
    nav.appendChild(label);
    nav.appendChild(next);
    el.appendChild(info);
    el.appendChild(sel);
    el.appendChild(nav);
  }

  // clampPage keeps a page index inside the available pages.
  function clampPage(page, total, size) {
    return Math.max(0, Math.min(page, Math.ceil(total / size) - 1));
  }

  // savedNumber reads a per-browser number preference.
  function savedNumber(key, def) {
    try { var v = Number(localStorage.getItem(key)); return v > 0 ? v : def; } catch (e) { return def; }
  }

  function saveNumber(key, v) {
    try { localStorage.setItem(key, String(v)); } catch (e) { /* storage unavailable */ }
  }

  // relTime says how long ago a time was: "just now", "5 min ago", "3 h ago",
  // falling back to the date.
  function relTime(iso) {
    var t = new Date(iso).getTime();
    if (!isFinite(t)) return "—";
    var s = (Date.now() - t) / 1000;
    if (s < 45) return "just now";
    if (s < 3600) return Math.round(s / 60) + " min ago";
    if (s < 86400) return Math.round(s / 3600) + " h ago";
    if (s < 7 * 86400) return Math.round(s / 86400) + " d ago";
    return new Date(t).toLocaleDateString();
  }

  function fmtNum(n) {
    n = Number(n) || 0;
    if (n >= 1e6) return (n / 1e6).toFixed(1).replace(/\.0$/, "") + "M";
    if (n >= 1e4) return Math.round(n / 1e3) + "k";
    if (n >= 1e3) return (n / 1e3).toFixed(1).replace(/\.0$/, "") + "k";
    return String(n);
  }

  // ---- Charts ----

  // drawBars renders hourly counts as stacked CSS bars (succeeded under
  // failed) with an hour axis; values are numbers, so no markup injection.
  function drawBars(el, hourly, withAxis) {
    el.innerHTML = "";
    var max = 1;
    hourly.forEach(function (h) { max = Math.max(max, h.requests); });
    var bars = document.createElement("div");
    bars.className = "bars";
    hourly.forEach(function (h) {
      var col = document.createElement("div");
      col.className = "bar-col";
      var when = new Date(h.hour * 1000);
      col.title = when.toLocaleString([], { weekday: "short", hour: "2-digit", minute: "2-digit" }) + "\n"
        + h.requests + " requests, " + h.failed + " failed\n"
        + fmtNum(h.input_tokens) + " in / " + fmtNum(h.output_tokens) + " out tokens"
        + (h.cached ? "\n" + h.cached + " from cache, " + fmtNum((h.saved_input_tokens || 0) + (h.saved_output_tokens || 0)) + " tokens saved" : "");
      var okH = (h.requests - h.failed) / max * 100, errH = h.failed / max * 100;
      var err = document.createElement("div");
      err.className = "bar-err";
      err.style.height = errH + "%";
      var ok = document.createElement("div");
      ok.className = "bar-ok";
      ok.style.height = okH + "%";
      col.appendChild(err);
      col.appendChild(ok);
      bars.appendChild(col);
    });
    el.appendChild(bars);
    if (!withAxis) return;
    var axis = document.createElement("div");
    axis.className = "bar-axis";
    hourly.forEach(function (h, i) {
      var t = document.createElement("span");
      if (i % 6 === 0 || i === hourly.length - 1) {
        t.appendChild(textNode(new Date(h.hour * 1000).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" })));
      }
      axis.appendChild(t);
    });
    el.appendChild(axis);
  }

  var lastHourly = [];

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
        { label: "Failed", val: (d.failed != null ? d.failed : 0), sub: since },
        { label: "Cache hits", val: cacheHitRate(d), sub: cacheSavings(d.cache) }
      ];
      lastCache = d.cache || null;
      renderCacheState();
      lastHourly = d.hourly || [];
      var day = lastHourly.reduce(function (n, h) { return n + h.requests; }, 0);
      items[2].sub = day + " in the last 24 h";
      items[2].spark = lastHourly;
      items.forEach(function (it) {
        var c = document.createElement("div");
        c.className = "stat-card";
        if (it.spark) {
          var sp = document.createElement("div");
          sp.className = "spark";
          drawBars(sp, it.spark, false);
          c.appendChild(sp);
          c.classList.add("has-spark");
        }
        [["stat-val", String(it.val)], ["stat-label", it.label], ["stat-sub", it.sub]].forEach(function (p) {
          if (!p[1]) return;
          var span = document.createElement("span");
          span.className = p[0];
          span.appendChild(textNode(p[1]));
          c.appendChild(span);
        });
        g.appendChild(c);
      });
      setNavCount("nav-count-accounts", d.accounts);
      document.getElementById("status-accounts").textContent =
        (d.enabled != null ? d.enabled : d.accounts) + " of " + d.accounts + " accounts enabled";
      setServerOnline(true);
    }).catch(function (e) {
      console.warn("loadStats:", e.message);
      if (!e.status) setServerOnline(false); // network error: server unreachable
    });
  }

  var lastCache = null;

  // cacheHitRate is the share of all requests answered from the cache.
  function cacheHitRate(d) {
    var c = d.cache || {};
    var hits = c.hits || 0, total = d.requests || 0;
    if (!total || !hits) return c.enabled === false ? "off" : "0%";
    return Math.round(hits / total * 100) + "%";
  }

  // cacheSavings describes what the cache has saved.
  function cacheSavings(c) {
    if (!c) return "";
    var saved = (c.saved_input_tokens || 0) + (c.saved_output_tokens || 0);
    return fmtNum(c.hits || 0) + " hits · " + fmtNum(saved) + " tokens saved";
  }

  // renderCacheState shows the cache's size under its settings.
  function renderCacheState() {
    var el = document.getElementById("cache-state");
    if (!el || !lastCache) return;
    var c = lastCache;
    el.textContent = fmtNum(c.entries || 0) + " answers · " + fmtMB(c.bytes || 0) + " · " + cacheSavings(c)
      + (c.enabled ? "" : " · off");
  }

  function fmtMB(b) {
    if (b >= 1 << 20) return (b / (1 << 20)).toFixed(1).replace(/\.0$/, "") + " MB";
    if (b >= 1024) return Math.round(b / 1024) + " KB";
    return b + " B";
  }

  function setServerOnline(on) {
    document.querySelector(".status-card").classList.toggle("offline", !on);
    document.querySelector(".status-line strong").textContent = on ? "Server online" : "Server unreachable";
  }

  function loadVersion() {
    apiGet("/admin/api/settings").then(function (s) {
      document.getElementById("status-version").textContent = s.version || "";
    }).catch(function () { /* shown elsewhere */ });
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

  var activeTab = "accounts";

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
          activeTab = name;
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

  var logCache = [];
  var logPage = 0;
  var logPageSize = savedNumber("devin2api-log-page", 50);
  var logOpen = {}; // expanded rows, by logKey
  var logTimer = null;

  function logKey(e) { return e.time + "|" + e.endpoint + "|" + e.account; }

  function loadLogs() {
    logLoading = true;
    Promise.all([apiGet("/admin/api/logs?limit=1000"), apiGet("/admin/api/stats")]).finally(function () { logLoading = false; }).then(function (r) {
      logCache = r[0] || [];
      lastHourly = r[1].hourly || [];
      renderActivity();
      fillEndpointFilter();
      drawLogs();
    }).catch(function (err) { toast(err.message, "#f87171"); });
  }

  // renderActivity draws the 24-hour chart, its totals, and the top models,
  // accounts and keys of the last 24 hours of the log.
  function renderActivity() {
    drawBars(document.getElementById("chart-24h"), lastHourly, true);
    var tot = { requests: 0, failed: 0, input_tokens: 0, output_tokens: 0, cached: 0, saved_input_tokens: 0, saved_output_tokens: 0 };
    lastHourly.forEach(function (h) { for (var k in tot) tot[k] += h[k] || 0; });
    var totals = document.getElementById("chart-totals");
    totals.innerHTML = "";
    [["Requests", fmtNum(tot.requests)],
     ["Failed", fmtNum(tot.failed) + (tot.requests ? " · " + Math.round(tot.failed / tot.requests * 100) + "%" : "")],
     ["Tokens in", fmtNum(tot.input_tokens)],
     ["Tokens out", fmtNum(tot.output_tokens)],
     ["Cached", fmtNum(tot.cached) + (tot.requests ? " · " + Math.round(tot.cached / tot.requests * 100) + "%" : "")],
     ["Tokens saved", fmtNum(tot.saved_input_tokens + tot.saved_output_tokens)]].forEach(function (p) {
      var d = document.createElement("div");
      var b = document.createElement("strong");
      b.appendChild(textNode(p[1]));
      var l = document.createElement("span");
      l.appendChild(textNode(p[0]));
      d.appendChild(b);
      d.appendChild(l);
      totals.appendChild(d);
    });
    var since = Date.now() - 86400000;
    var day = logCache.filter(function (e) { return new Date(e.time).getTime() >= since; });
    drawTop("top-models", day, function (e) { return e.upstream_model || e.model; });
    drawTop("top-accounts", day, function (e) { return e.account_label || e.account; });
    drawTop("top-keys", day, function (e) { return e.key || (e.endpoint ? "no key (local)" : ""); });
  }

  function drawTop(id, entries, keyFn) {
    var groups = {};
    entries.forEach(function (e) {
      var k = keyFn(e);
      if (!k) return;
      var g = groups[k] || (groups[k] = { name: k, n: 0, err: 0 });
      g.n++;
      if (e.status !== 200) g.err++;
    });
    var list = Object.keys(groups).map(function (k) { return groups[k]; })
      .sort(function (a, b) { return b.n - a.n; }).slice(0, 5);
    var ol = document.getElementById(id);
    ol.innerHTML = "";
    if (!list.length) {
      var li = document.createElement("li");
      li.className = "top-empty";
      li.appendChild(textNode("No requests in the last 24 h"));
      ol.appendChild(li);
      return;
    }
    list.forEach(function (g) {
      var li = document.createElement("li");
      var row = document.createElement("div");
      row.className = "top-row";
      var name = document.createElement("span");
      name.className = "top-name";
      name.title = g.name;
      name.appendChild(textNode(g.name));
      var n = document.createElement("span");
      n.className = "top-n";
      n.appendChild(textNode(g.n + (g.err ? " · " + g.err + " failed" : "")));
      row.appendChild(name);
      row.appendChild(n);
      var track = document.createElement("div");
      track.className = "top-track";
      var bar = document.createElement("div");
      bar.className = "top-bar";
      bar.style.width = (g.n / list[0].n * 100) + "%";
      if (g.err) {
        var eb = document.createElement("div");
        eb.className = "top-bar-err";
        eb.style.width = (g.err / g.n * 100) + "%";
        bar.appendChild(eb);
      }
      track.appendChild(bar);
      li.appendChild(row);
      li.appendChild(track);
      ol.appendChild(li);
    });
  }

  function fillEndpointFilter() {
    var sel = document.getElementById("log-endpoint");
    var cur = sel.value;
    var seen = {};
    logCache.forEach(function (e) { seen[e.endpoint] = true; });
    while (sel.options.length > 1) sel.remove(1);
    Object.keys(seen).sort().forEach(function (ep) {
      var o = document.createElement("option");
      o.value = ep;
      o.textContent = ep;
      sel.appendChild(o);
    });
    sel.value = seen[cur] ? cur : "";
  }

  function visibleLogs() {
    var q = document.getElementById("log-search").value.trim().toLowerCase();
    var st = document.getElementById("log-status").value;
    var ep = document.getElementById("log-endpoint").value;
    return logCache.filter(function (e) {
      if (st === "ok" && e.status !== 200) return false;
      if (st === "err" && e.status === 200) return false;
      if (ep && e.endpoint !== ep) return false;
      if (!q) return true;
      return [e.model, e.upstream_model, e.account_label, e.account, e.key, e.error, e.endpoint, e.finish, e.cached ? "cache" : ""].some(function (v) {
        return v && String(v).toLowerCase().indexOf(q) !== -1;
      });
    });
  }

  function drawLogs() {
    var rows = visibleLogs();
    logPage = clampPage(logPage, rows.length, logPageSize);
    var tbody = document.querySelector("#log-table tbody");
    tbody.innerHTML = "";
    if (!logCache.length) emptyRow(tbody, 8, "No chat requests yet.");
    else if (!rows.length) emptyRow(tbody, 8, "No requests match the search or filter.");
    rows.slice(logPage * logPageSize, (logPage + 1) * logPageSize).forEach(function (e) {
      var key = logKey(e);
      var tr = document.createElement("tr");
      tr.className = "log-row" + (logOpen[key] ? " open" : "");
      var t = new Date(e.time);
      var cells = [
        t.toLocaleDateString() === new Date().toLocaleDateString() ? t.toLocaleTimeString() : t.toLocaleString(),
        e.endpoint,
        e.model === e.upstream_model ? e.model : e.model + " → " + e.upstream_model,
        e.cached ? "cache" : (e.account_label || e.account || "—"),
        e.key || "—",
        String(e.status),
        fmtMs(e.duration_ms),
        (e.input_tokens || 0) + " / " + (e.output_tokens || 0)
      ];
      cells.forEach(function (c, i) {
        var td = document.createElement("td");
        if (i === 5) {
          var b = document.createElement("span");
          b.className = "badge " + (e.status === 200 ? "badge-active" : "badge-dead");
          b.appendChild(textNode(c));
          td.appendChild(b);
          if (e.error) td.title = e.error;
        } else if (i === 3 && e.cached) {
          var cb = document.createElement("span");
          cb.className = "badge badge-cache";
          cb.title = "Answered from the response cache; no account usage spent";
          cb.appendChild(textNode(c));
          td.appendChild(cb);
        } else {
          td.appendChild(textNode(c));
        }
        tr.appendChild(td);
      });
      tr.onclick = function () {
        if (logOpen[key]) delete logOpen[key]; else logOpen[key] = true;
        drawLogs();
      };
      tbody.appendChild(tr);
      if (logOpen[key]) tbody.appendChild(logDetailRow(e));
    });
    var count = document.getElementById("log-count");
    count.textContent = rows.length === logCache.length
      ? logCache.length + " request" + (logCache.length === 1 ? "" : "s")
      : rows.length + " of " + logCache.length + " shown";
    renderPager(document.getElementById("log-pager"), rows.length, logPage, logPageSize, function (p, size) {
      logPage = p;
      if (size !== logPageSize) { logPageSize = size; saveNumber("devin2api-log-page", size); }
      drawLogs();
    });
  }

  // logDetailRow lists every field of one request under its row.
  function logDetailRow(e) {
    var tr = document.createElement("tr");
    tr.className = "log-detail";
    var td = document.createElement("td");
    td.colSpan = 8;
    var dl = document.createElement("dl");
    dl.className = "detail-grid";
    [
      ["Time", new Date(e.time).toLocaleString()],
      ["Endpoint", e.endpoint + (e.stream ? " (stream)" : "")],
      ["Model requested", e.model],
      ["Upstream model", e.upstream_model],
      ["Account", e.cached ? "response cache (no usage spent)" : (e.account_label || "—") + (e.account && e.account !== e.account_label ? " · " + e.account : "")],
      ["API key", e.key || "none (local client)"],
      ["Attempts", String(e.attempts || 0) + (e.attempts > 1 ? " (failover)" : "")],
      ["Status", String(e.status)],
      ["Duration", fmtMs(e.duration_ms)],
      ["First output", e.first_output_ms ? fmtMs(e.first_output_ms) : "—"],
      ["Tokens in / out", (e.input_tokens || 0) + " / " + (e.output_tokens || 0) + (e.cached ? " (saved)" : "")],
      ["Tool calls", String(e.tool_calls || 0)],
      ["Finish", e.finish || "—"],
      ["Error", e.error || "—"]
    ].forEach(function (p) {
      var dt = document.createElement("dt");
      dt.appendChild(textNode(p[0]));
      var dd = document.createElement("dd");
      dd.appendChild(textNode(p[1]));
      var item = document.createElement("div");
      item.className = "detail-item" + (p[0] === "Error" && e.error ? " detail-err" : "");
      item.appendChild(dt);
      item.appendChild(dd);
      dl.appendChild(item);
    });
    td.appendChild(dl);
    tr.appendChild(td);
    return tr;
  }

  // setLogAuto turns the 5-second refresh on or off; it only fetches while
  // the Logs tab is open.
  // canPoll tells a background timer whether to fetch: not while the page is
  // hidden or the admin login is showing (a 401 would only repeat).
  function canPoll() {
    return !document.hidden && document.getElementById("login-overlay").classList.contains("hidden");
  }

  var logLoading = false;

  function setLogAuto(on) {
    if (logTimer) { clearInterval(logTimer); logTimer = null; }
    if (on) logTimer = setInterval(function () {
      // Skip a tick while the previous load is still running.
      if (activeTab === "logs" && canPoll() && !logLoading) loadLogs();
    }, 5000);
    try { localStorage.setItem("devin2api-log-auto", on ? "1" : ""); } catch (e) { /* storage unavailable */ }
  }

  function setupLogs() {
    var redraw = function () { logPage = 0; drawLogs(); };
    document.getElementById("log-search").oninput = redraw;
    document.getElementById("log-status").onchange = redraw;
    document.getElementById("log-endpoint").onchange = redraw;
    document.getElementById("btn-refresh-logs").onclick = loadLogs;
    var auto = document.getElementById("logs-auto");
    try { auto.checked = localStorage.getItem("devin2api-log-auto") === "1"; } catch (e) { /* storage unavailable */ }
    auto.onchange = function () { setLogAuto(auto.checked); };
    setLogAuto(auto.checked);
    document.getElementById("btn-clear-logs").onclick = function () {
      confirmDialog("Clear the request log?", "All " + logCache.length + " logged requests are deleted. Totals and the hourly chart are kept.", "Clear log")
        .then(function (yes) {
          if (!yes) return;
          apiDelete("/admin/api/logs").then(function () { logOpen = {}; loadLogs(); toast("Request log cleared", "#34d399"); })
            .catch(function (e) { toast(e.message, "#f87171"); });
        });
    };
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
  var acctPage = 0;
  var acctPageSize = savedNumber("devin2api-acct-page", 25);

  // pageAccounts is the slice of visible accounts on the current page.
  function pageAccounts(vis) {
    acctPage = clampPage(acctPage, vis.length, acctPageSize);
    return vis.slice(acctPage * acctPageSize, (acctPage + 1) * acctPageSize);
  }

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
    var vis = visibleAccounts();
    var accs = pageAccounts(vis);
    var tbody = document.querySelector("#acct-table tbody");
    tbody.innerHTML = "";
    if (!acctCache.length) emptyRow(tbody, 8, "No accounts yet. Use \"Add account\" to log in or import one.");
    else if (!vis.length) emptyRow(tbody, 8, "No accounts match the search or filter.");
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
          confirmDialog("Delete this account?", label + " is removed from the pool, with its session token. This cannot be undone.", "Delete account")
            .then(function (yes) {
              if (!yes) return;
              apiDelete("/admin/api/accounts/" + id)
                .then(function () { loadAccounts(); loadStats(); })
                .catch(function (e) { toast(e.message, "#f87171"); });
            });
        };
      })(a.id, a.nickname || identityCell(a));
      tdAct.appendChild(delBtn);
      tr.appendChild(tdAct);

      tbody.appendChild(tr);
    }

    var count = document.getElementById("acct-count");
    count.textContent = vis.length === acctCache.length
      ? acctCache.length + " account" + (acctCache.length === 1 ? "" : "s")
      : vis.length + " of " + acctCache.length + " match";
    renderPager(document.getElementById("acct-pager"), vis.length, acctPage, acctPageSize, function (p, size) {
      acctPage = p;
      if (size !== acctPageSize) { acctPageSize = size; saveNumber("devin2api-acct-page", size); }
      drawAccounts();
    });
    updateBulkBar();
  }

  // updateBulkBar shows the bulk actions while anything is selected, offers
  // to extend the selection to every matching account, and keeps the header
  // checkbox in step with the rows on the page.
  function updateBulkBar() {
    var ids = selectedIDs();
    var bar = document.getElementById("bulk-bar");
    bar.classList.toggle("hidden", ids.length === 0);
    document.getElementById("bulk-count").textContent = ids.length + " selected";
    var vis = visibleAccounts();
    var unselected = vis.filter(function (a) { return !selected[a.id]; }).length;
    var more = document.getElementById("bulk-select-all");
    more.classList.toggle("hidden", unselected === 0);
    more.textContent = "Select all " + vis.length + " matching";
    var page = pageAccounts(vis);
    var nSel = page.filter(function (a) { return selected[a.id]; }).length;
    var all = document.getElementById("acct-check-all");
    all.checked = page.length > 0 && nSel === page.length;
    all.indeterminate = nSel > 0 && nSel < page.length;
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
    // A new search or filter drops selected accounts it hides, so a bulk
    // action never touches rows the user cannot see.
    var redraw = function () {
      acctPage = 0;
      var vis = {};
      visibleAccounts().forEach(function (a) { vis[a.id] = true; });
      Object.keys(selected).forEach(function (id) { if (!vis[id]) delete selected[id]; });
      drawAccounts();
    };
    document.getElementById("acct-search").oninput = redraw;
    document.getElementById("acct-filter").onchange = redraw;
    document.getElementById("acct-check-all").onchange = function (ev) {
      pageAccounts(visibleAccounts()).forEach(function (a) {
        if (ev.target.checked) selected[a.id] = true; else delete selected[a.id];
      });
      drawAccounts();
    };
    document.getElementById("bulk-select-all").onclick = function () {
      visibleAccounts().forEach(function (a) { selected[a.id] = true; });
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
      if (!ids.length) return;
      var btn = this;
      var n = ids.length + " account" + (ids.length === 1 ? "" : "s");
      confirmDialog("Delete " + n + "?", "The selected accounts are removed from the pool, with their session tokens. This cannot be undone.", "Delete " + n)
        .then(function (yes) {
          if (!yes) return;
          bulkRun(btn, "Deleting", ids, function (id) {
            return apiDelete("/admin/api/accounts/" + id).then(function () { delete selected[id]; });
          });
        });
    };
  }

  // ---- Tokens (API Keys) tab ----

  // keyState is a key's status for the badge: disabled, expired or out of
  // quota keys are refused by the server.
  function keyState(t) {
    if (t.disabled) return { label: "Disabled", cls: "badge badge-disabled" };
    if (t.expires_at && new Date(t.expires_at).getTime() <= Date.now()) return { label: "Expired", cls: "badge badge-expired" };
    if (t.quota && t.requests >= t.quota) return { label: "Quota used", cls: "badge badge-dead" };
    return { label: "Active", cls: "badge badge-active" };
  }

  function td(child, cls) {
    var c = document.createElement("td");
    if (cls) c.className = cls;
    if (child != null) c.appendChild(typeof child === "string" ? textNode(child) : child);
    return c;
  }

  function span(cls, txt) {
    var el = document.createElement("span");
    el.className = cls;
    el.appendChild(textNode(txt));
    return el;
  }

  var tokCache = [];

  function renderTokens(toks) {
    tokCache = toks;
    var tbody = document.querySelector("#tok-table tbody");
    tbody.innerHTML = "";
    setNavCount("nav-count-tokens", toks.length);
    if (!toks.length) emptyRow(tbody, 7, "No API keys. Without keys, only local clients may call /v1.");
    toks.forEach(function (t) {
      var tr = document.createElement("tr");

      var ident = document.createElement("div");
      ident.className = "ident-text";
      var name = span("ident-main" + (t.name ? "" : " unnamed"), t.name || "Unnamed key");
      ident.appendChild(name);
      ident.appendChild(span("ident-sub", t.hint || t.id));
      tr.appendChild(td(ident));

      var st = keyState(t);
      tr.appendChild(td(span(st.cls, st.label)));

      var req = document.createElement("div");
      req.className = "key-usage";
      if (t.quota) {
        var used = Math.min(100, t.requests / t.quota * 100);
        var track = document.createElement("div");
        track.className = "usage-bar-track";
        var bar = document.createElement("div");
        bar.className = "usage-bar" + (used >= 90 ? " critical" : used >= 70 ? " high" : "");
        bar.style.width = used + "%";
        track.appendChild(bar);
        req.appendChild(track);
        req.appendChild(span("key-usage-text", fmtNum(t.requests) + " / " + fmtNum(t.quota)));
      } else {
        req.appendChild(span("key-usage-text", fmtNum(t.requests || 0) + " · no limit"));
      }
      tr.appendChild(td(req));

      var models = document.createElement("div");
      models.className = "chips";
      if (!t.models || !t.models.length) {
        models.appendChild(span("muted", "All models"));
      } else {
        t.models.slice(0, 2).forEach(function (m) { models.appendChild(span("chip", m)); });
        if (t.models.length > 2) {
          var more = span("chip chip-more", "+" + (t.models.length - 2));
          more.title = t.models.slice(2).join(", ");
          models.appendChild(more);
        }
      }
      tr.appendChild(td(models));

      tr.appendChild(td(t.expires_at ? new Date(t.expires_at).toLocaleDateString() : "Never"));
      var lu = td(t.last_used ? relTime(t.last_used) : "Never");
      if (t.last_used) lu.title = new Date(t.last_used).toLocaleString();
      tr.appendChild(lu);

      var act = td(null);
      var edit = document.createElement("button");
      edit.className = "btn-sm";
      edit.textContent = "Edit";
      edit.onclick = function () { openKeyDialog(t); };
      var toggle = document.createElement("button");
      toggle.className = "btn-sm";
      toggle.textContent = t.disabled ? "Enable" : "Disable";
      toggle.onclick = function () {
        var body = keyOptions(t);
        body.disabled = !t.disabled;
        apiPut("/admin/api/tokens/" + t.id, JSON.stringify(body)).then(loadTokens)
          .catch(function (e) { toast(e.message, "#f87171"); });
      };
      var rev = document.createElement("button");
      rev.className = "btn-sm btn-del";
      rev.textContent = "Revoke";
      rev.onclick = function () {
        confirmDialog("Revoke this API key?", (t.name || t.hint || t.id) + " stops working immediately. Clients using it get 401 errors.", "Revoke key")
          .then(function (yes) {
            if (!yes) return;
            apiDelete("/admin/api/tokens/" + t.id).then(loadTokens)
              .catch(function (e) { toast(e.message, "#f87171"); });
          });
      };
      act.appendChild(edit);
      act.appendChild(toggle);
      act.appendChild(rev);
      tr.appendChild(act);
      tbody.appendChild(tr);
    });
  }

  function loadTokens() {
    apiGet("/admin/api/tokens").then(renderTokens).catch(function (e) { toast(e.message, "#f87171"); });
  }

  // keyOptions is the editable part of a key, as PUT expects it.
  function keyOptions(t) {
    return { name: t.name || "", disabled: !!t.disabled, expires_at: t.expires_at || null, models: t.models || [], quota: t.quota || 0 };
  }

  var catalog = null; // admin model list, loaded on first use
  var editingKey = null;

  function dateInputValue(iso) {
    if (!iso) return "";
    var d = new Date(iso);
    var p = function (n) { return (n < 10 ? "0" : "") + n; };
    return d.getFullYear() + "-" + p(d.getMonth() + 1) + "-" + p(d.getDate());
  }

  // renderModelPicks lists the catalog as checkboxes, plus any model the
  // key allows that the catalog no longer has.
  function renderModelPicks(chosen) {
    var box = document.getElementById("key-models");
    box.innerHTML = "";
    var list = (catalog && catalog.models || []).slice();
    chosen.forEach(function (id) {
      if (!list.some(function (m) { return m.id === id; })) list.push({ id: id, label: "" });
    });
    if (!list.length) {
      box.appendChild(span("muted model-empty", "The model list loads once an account can reach Devin. Add model ids below."));
      return;
    }
    list.forEach(function (m) {
      var l = document.createElement("label");
      l.className = "model-pick";
      var c = document.createElement("input");
      c.type = "checkbox";
      c.value = m.id;
      c.checked = chosen.indexOf(m.id) !== -1;
      l.appendChild(c);
      var txt = document.createElement("span");
      txt.appendChild(span("mono", m.id));
      if (m.label && m.label !== m.id) txt.appendChild(span("muted", m.label));
      l.appendChild(txt);
      box.appendChild(l);
    });
  }

  function openKeyDialog(t) {
    editingKey = t;
    var editing = !!t;
    document.getElementById("key-title").textContent = editing ? "Edit API key" : "Create API key";
    document.getElementById("key-submit").textContent = editing ? "Save changes" : "Create key";
    document.getElementById("key-name").value = editing ? t.name || "" : "";
    document.getElementById("key-expires").value = editing ? dateInputValue(t.expires_at) : "";
    document.getElementById("key-quota").value = editing && t.quota ? t.quota : "";
    var chosen = editing && t.models ? t.models : [];
    document.getElementById("key-all-models").checked = !chosen.length;
    document.getElementById("key-models-wrap").classList.toggle("hidden", !chosen.length);
    document.getElementById("key-disabled").checked = editing && !!t.disabled;
    document.getElementById("key-reset").checked = false;
    document.getElementById("key-reset-label").textContent = "Reset the request counter" + (editing ? " (" + (t.requests || 0) + " so far)" : "");
    document.getElementById("key-status").textContent = "";
    var edits = document.querySelectorAll("#key-form .edit-only");
    for (var i = 0; i < edits.length; i++) edits[i].classList.toggle("hidden", !editing);
    renderModelPicks(chosen);
    showDialog("key-overlay");
    document.getElementById("key-name").focus();
    if (!catalog) {
      apiGet("/admin/api/models").then(function (c) {
        catalog = c;
        renderModelPicks(checkedModels());
      }).catch(function () { /* the list stays empty */ });
    }
  }

  function checkedModels() {
    var out = [];
    var boxes = document.querySelectorAll("#key-models input");
    for (var i = 0; i < boxes.length; i++) if (boxes[i].checked) out.push(boxes[i].value);
    return out;
  }

  function submitKeyForm(ev) {
    ev.preventDefault();
    var status = document.getElementById("key-status");
    var exp = document.getElementById("key-expires").value;
    var quota = document.getElementById("key-quota").value;
    var body = {
      name: document.getElementById("key-name").value.trim(),
      // A date means "through the end of that day", in local time.
      expires_at: exp ? new Date(exp + "T23:59:59").toISOString() : null,
      models: document.getElementById("key-all-models").checked ? [] : checkedModels(),
      quota: quota ? Math.max(0, Math.floor(Number(quota))) : 0,
      disabled: !!editingKey && document.getElementById("key-disabled").checked
    };
    if (!document.getElementById("key-all-models").checked && !body.models.length) {
      status.textContent = "Pick at least one model, or allow every model.";
      return;
    }
    var btn = document.getElementById("key-submit");
    btn.disabled = true;
    var req;
    if (editingKey) {
      body.reset_requests = document.getElementById("key-reset").checked;
      req = apiPut("/admin/api/tokens/" + editingKey.id, JSON.stringify(body));
    } else {
      req = apiPost("/admin/api/tokens", JSON.stringify(body));
    }
    req.then(function (tok) {
      var created = !editingKey;
      if (dialogClose) dialogClose();
      loadTokens();
      if (created) revealKey(tok.token);
      else toast("API key saved", "#34d399");
    }).catch(function (e) {
      status.textContent = e.message;
    }).then(function () { btn.disabled = false; });
  }

  // revealKey shows a new key's value once, with a copy button.
  function revealKey(value) {
    document.getElementById("reveal-value").textContent = value;
    // Sticky: the value is shown once, so only Done may close this.
    showDialog("reveal-overlay", { sticky: true, onClose: function () { document.getElementById("reveal-value").textContent = ""; } });
    document.getElementById("reveal-copy").focus();
  }

  function copyText(text) {
    if (navigator.clipboard && window.isSecureContext) return navigator.clipboard.writeText(text);
    // Plain-HTTP pages (a LAN address) have no clipboard API.
    var ta = document.createElement("textarea");
    ta.value = text;
    ta.style.position = "fixed";
    ta.style.opacity = "0";
    document.body.appendChild(ta);
    ta.select();
    var ok = document.execCommand("copy");
    ta.remove();
    return ok ? Promise.resolve() : Promise.reject(new Error("copy failed"));
  }

  function setupKeys() {
    document.getElementById("btn-create-token").onclick = function () { openKeyDialog(null); };
    document.getElementById("key-form").onsubmit = submitKeyForm;
    document.getElementById("key-close").onclick = function () { if (dialogClose) dialogClose(); };
    document.getElementById("key-cancel").onclick = function () { if (dialogClose) dialogClose(); };
    document.getElementById("key-all-models").onchange = function (ev) {
      document.getElementById("key-models-wrap").classList.toggle("hidden", ev.target.checked);
    };
    var addModel = function () {
      var input = document.getElementById("key-model-add");
      var id = input.value.trim();
      if (!id) return;
      var chosen = checkedModels();
      if (chosen.indexOf(id) === -1) chosen.push(id);
      renderModelPicks(chosen);
      input.value = "";
      input.focus();
    };
    document.getElementById("key-model-add-btn").onclick = addModel;
    document.getElementById("key-model-add").onkeydown = function (ev) {
      if (ev.key === "Enter") { ev.preventDefault(); addModel(); }
    };
    document.getElementById("reveal-copy").onclick = function () {
      copyText(document.getElementById("reveal-value").textContent)
        .then(function () { toast("Key copied", "#34d399"); })
        .catch(function () { toast("Copy failed: select the key and copy it by hand", "#f87171"); });
    };
    document.getElementById("reveal-done").onclick = function () { if (dialogClose) dialogClose(); };
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
      system_prompt_mode: s.system_prompt_mode || "prepend",
      cache_enabled: !!s.cache_enabled,
      cache_ttl_minutes: s.cache_ttl_minutes || 0,
      cache_max_entries: s.cache_max_entries || 0,
      cache_max_mb: s.cache_max_mb || 0
    };
  }

  // Cache number fields: 0 means the default and shows as an empty field.
  var cacheFields = [["cache-ttl", "cache_ttl_minutes"], ["cache-entries", "cache_max_entries"], ["cache-mb", "cache_max_mb"]];

  function renderCacheForm() {
    document.getElementById("cache-enabled").checked = draft.cache_enabled;
    cacheFields.forEach(function (f) {
      var el = document.getElementById(f[0]);
      el.value = draft[f[1]] ? String(draft[f[1]]) : "";
      el.disabled = !draft.cache_enabled;
    });
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
    renderCacheForm();
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
    // draft is null until the settings have loaded; until then the controls
    // have nothing to edit.
    document.getElementById("btn-upstream-add").onclick = function () {
      if (!draft) return;
      // The first added row starts from the default so the order is explicit.
      if (!draft.upstreams.length && defaultUpstream) draft.upstreams.push(defaultUpstream);
      draft.upstreams.push("");
      renderUpstreams();
      markDirty();
      var inputs = document.querySelectorAll("#upstream-list input");
      if (inputs.length) inputs[inputs.length - 1].focus();
    };
    document.getElementById("sp-enabled").onchange = function (ev) {
      if (!draft) return;
      draft.system_prompt_enabled = ev.target.checked; updatePromptCount(); markDirty();
    };
    document.getElementById("sp-mode").onchange = function (ev) {
      if (!draft) return;
      draft.system_prompt_mode = ev.target.value; markDirty();
    };
    document.getElementById("sp-text").oninput = function (ev) {
      if (!draft) return;
      draft.system_prompt = ev.target.value; updatePromptCount(); markDirty();
    };
    document.getElementById("cache-enabled").onchange = function (ev) {
      if (!draft) return;
      draft.cache_enabled = ev.target.checked; renderCacheForm(); markDirty();
    };
    cacheFields.forEach(function (f) {
      document.getElementById(f[0]).oninput = function (ev) {
        if (!draft) return;
        // Number, not parseInt: a number input accepts "1e3", which parseInt
        // would read as 1.
        var n = Math.floor(Number(ev.target.value));
        draft[f[1]] = isNaN(n) || n < 0 ? 0 : n;
        markDirty();
      };
    });
    document.getElementById("btn-cache-clear").onclick = function () {
      var n = lastCache ? (lastCache.entries || 0) : 0;
      confirmDialog("Clear the response cache?", "All " + n + " cached answers are dropped; the next identical requests go upstream again. Hit totals are kept.", "Clear cache")
        .then(function (yes) {
          if (!yes) return;
          apiDelete("/admin/api/cache").then(function () { toast("Response cache cleared", "#34d399"); loadStats(); })
            .catch(function (e) { toast(e.message, "#f87171"); });
        });
    };
    document.getElementById("btn-settings-save").onclick = saveSettings;
    document.getElementById("btn-settings-reset").onclick = function () {
      draft = editable(savedSettings);
      renderUpstreams();
      renderPromptForm();
      renderCacheForm();
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
    document.getElementById("import-status").textContent = "";
    document.getElementById("import-ta").value = "";
    document.getElementById("manual-status").textContent = "";
    document.getElementById("manual-token").value = "";
    document.getElementById("login-area").className = "login-area hidden";
    // Closing the dialog while a browser login is pending cancels it on
    // the server too, so it does not stay pending in the background.
    showDialog("modal-overlay", { onClose: function () { if (loginPollInterval) cancelLogin(); } });
  }

  function closeModal() {
    var el = document.getElementById("modal-overlay");
    if (dialogClose && dialogClose.el === el) {
      dialogClose();
      return;
    }
    el.classList.add("hidden");
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
      ["/v1/responses", "POST", "OpenAI Responses API (previous_response_id kept 24 h in memory, per API key)"],
      ["/v1/completions", "POST", "Legacy text completions, emulated on the chat models"],
      ["/admin/api/accounts", "GET / POST", "List or import accounts"],
      ["/admin/api/stats", "GET", "Totals, cache hits and savings, plus request counts for each of the last 24 hours"],
      ["/admin/api/cache", "GET / DELETE", "Response cache state (entries, size, hits, tokens saved); DELETE drops every cached answer"],
      ["/admin/api/logs", "GET / DELETE", "Last 1000 chat requests (metadata only), newest first; DELETE clears them"],
      ["/admin/api/tokens", "GET / POST", "List API keys, or create one with a name, expiry, model list and quota"],
      ["/admin/api/tokens/{id}", "PUT / DELETE", "Edit or revoke an API key"]
    ];
    for (var i = 0; i < entries.length; i++) {
      var dt = document.createElement("dt");
      var method = document.createElement("span");
      method.className = entries[i][1] === "GET" ? "method" : "method post";
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
    loadTokens(); // for the sidebar count
    loadVersion();
    // After a re-login the open tab may be stale or empty.
    if (activeTab !== "accounts" && activeTab !== "tokens" && tabLoaders[activeTab]) tabLoaders[activeTab]();
  }

  // The status card only knows what the last stats request saw, so poll
  // it; a failed poll marks the server unreachable.
  function setupStatusPoll() {
    setInterval(function () { if (canPoll()) loadStats(); }, 30000);
  }

  function boot() {
    setupTheme();
    setupTabs();
    setupBulk();
    setupSettings();
    setupDialogs();
    setupLogs();
    setupKeys();
    setupStatusPoll();
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
    renderApiTab();
  }

  boot();
})();
