// admin panel shared JS — zero external dependencies, no console logging.
(function () {
  "use strict";

  /* ------------------------------------------------------------------ */
  /* API helpers                                                         */
  /* ------------------------------------------------------------------ */

  // apiPost POSTs body (string) to path and returns the parsed JSON value.
  // ADMIN_HEADERS marks requests as coming from this panel; the server rejects
  // state-changing admin calls without it (CSRF guard).
  var ADMIN_HEADERS = { "X-Devin2API": "1" };

  function apiPost(path, body) {
    return fetch(path, { method: "POST", headers: { "Content-Type": "application/json", "X-Devin2API": "1" }, body: body })
      .then(checkStatus)
      .then(function (r) { return r.json(); });
  }

  // apiGet GETs path and returns the parsed JSON value.
  function apiGet(path) {
    return fetch(path).then(checkStatus).then(function (r) { return r.json(); });
  }

  // apiDelete DELETEs path; resolves on success or rejects with the error body.
  function apiDelete(path) {
    return fetch(path, { method: "DELETE", headers: ADMIN_HEADERS }).then(checkStatus);
  }

  // checkStatus throws on non-2xx responses.
  function checkStatus(r) {
    if (r.ok) return r;
    return r.json().catch(function () { return { error: "HTTP " + r.status }; }).then(function (body) {
      var e = new Error(body.error || "request failed");
      e.status = r.status;
      throw e;
    });
  }

  /* ------------------------------------------------------------------ */
  /* Status line                                                         */
  /* ------------------------------------------------------------------ */

  function setStatus(el, text, ok) {
    el.textContent = text;
    el.className = ok ? "status-ok" : "status-err";
  }

  /* ------------------------------------------------------------------ */
  /* Accounts page                                                       */
  /* ------------------------------------------------------------------ */

  function initAccounts() {
    var table = document.querySelector("#acct-table tbody");
    var statusEl = document.getElementById("status");

    // renderAccounts renders the account list rows.
    function renderAccounts(accs) {
      table.innerHTML = "";
      accs.forEach(function (a) {
        var tr = document.createElement("tr");
        var expDate = a.tokens && a.tokens.expires_at
          ? new Date(a.tokens.expires_at * 1000).toLocaleString()
          : "—";
        var expired = a.expired;

        makeCell(tr, a.email || "—");
        makeCell(tr, a.sub || "—");
        makeCell(tr, expDate);
        makeBadge(tr, expired ? "expired" : "active", expired ? "badge-expired" : "badge-active");

        var act = document.createElement("td");
        var refBtn = document.createElement("button");
        refBtn.textContent = "Refresh";
        refBtn.onclick = function () {
          apiPost("/admin/api/accounts/" + a.id + "/refresh")
            .then(loadAccounts)
            .catch(function (e) { setStatus(statusEl, e.message, false); });
        };
        var delBtn = document.createElement("button");
        delBtn.textContent = "Delete";
        delBtn.onclick = function () {
          apiDelete("/admin/api/accounts/" + a.id)
            .then(loadAccounts)
            .catch(function (e) { setStatus(statusEl, e.message, false); });
        };
        act.appendChild(refBtn);
        act.appendChild(delBtn);
        tr.appendChild(act);
        table.appendChild(tr);
      });
    }

    // loadAccounts fetches and renders.
    function loadAccounts() { apiGet("/admin/api/accounts").then(renderAccounts).catch(function (e) { setStatus(statusEl, e.message, false); }); }

    // Import button handler.
    document.getElementById("import-btn").onclick = function () {
      var raw = document.getElementById("import-ta").value.trim();
      if (!raw) return setStatus(statusEl, "paste JSON first", false);
      apiPost("/admin/api/accounts", raw).then(loadAccounts).catch(function (e) { setStatus(statusEl, e.message, false); });
    };

    // Login button handler.
    document.getElementById("login-btn").onclick = function () {
      var email = document.getElementById("login-email").value.trim();
      // Passwords may legitimately start or end with spaces; don't trim.
      var pass = document.getElementById("login-pass").value;
      if (!email || !pass) return setStatus(statusEl, "fill both fields", false);
      apiPost("/admin/api/accounts", JSON.stringify({ email: email, password: pass }))
        .then(loadAccounts)
        .catch(function (e) { setStatus(statusEl, e.message, false); });
    };

    loadAccounts();
  }

  /* ------------------------------------------------------------------ */
  /* Tokens page                                                         */
  /* ------------------------------------------------------------------ */

  function initTokens() {
    var table = document.querySelector("#tok-table tbody");
    var statusEl = document.getElementById("status");
    var createdMsg = document.getElementById("created-msg");
    var currentList = [];

    function renderTokens(toks) {
      table.innerHTML = "";
      toks.forEach(function (t) {
        var tr = document.createElement("tr");
        makeCell(tr, t.id);
        makeCell(tr, t.created_at ? new Date(t.created_at).toLocaleString() : "—");
        makeCell(tr, t.last_used ? new Date(t.last_used).toLocaleString() : "never");

        var act = document.createElement("td");
        var revBtn = document.createElement("button");
        revBtn.textContent = "Revoke";
        revBtn.onclick = function () {
          apiDelete("/admin/api/tokens/" + t.id)
            .then(function () {
              currentList = currentList.filter(function (x) { return x.id !== t.id; });
              renderTokens(currentList);
            })
            .catch(function (e) { setStatus(statusEl, e.message, false); });
        };
        act.appendChild(revBtn);
        tr.appendChild(act);
        table.appendChild(tr);
      });
    }

    document.getElementById("create-btn").onclick = function () {
      createdMsg.textContent = "";
      apiPost("/admin/api/tokens", "")
        .then(function (tok) {
          currentList.unshift(tok);
          renderTokens(currentList);
          var msg = 'Created ' + tok.token + '\n\n⚠️ Shown once — copy it now!';
          createdMsg.textContent = msg;
          createdMsg.className = "created-box";
        })
        .catch(function (e) { setStatus(statusEl, e.message, false); });
    };

    apiGet("/admin/api/tokens").then(function (toks) {
      currentList = toks;
      renderTokens(toks);
    }).catch(function (e) { setStatus(statusEl, e.message, false); });
  }

  /* ------------------------------------------------------------------ */
  /* Settings page                                                       */
  /* ------------------------------------------------------------------ */

  function initSettings() {
    var statusEl = document.getElementById("status");
    apiGet("/admin/api/settings")
      .then(function (s) {
        document.getElementById("s-listen").value = s.listen || "";
        document.getElementById("s-data-dir").value = s.data_dir || "";
        document.getElementById("s-version").value = s.version || "";
      })
      .catch(function (e) { setStatus(statusEl, e.message, false); });
  }

  /* ------------------------------------------------------------------ */
  /* Shared DOM helpers                                                  */
  /* ------------------------------------------------------------------ */

  function makeCell(parent, text) {
    var td = document.createElement("td");
    td.textContent = String(text);
    parent.appendChild(td);
  }

  function makeBadge(parent, text, cls) {
    var td = document.createElement("td");
    var span = document.createElement("span");
    span.className = cls;
    span.textContent = text;
    td.appendChild(span);
    parent.appendChild(td);
  }

  /* ------------------------------------------------------------------ */
  /* Bootstrap                                                           */
  /* ------------------------------------------------------------------ */

  var page = document.body.getAttribute("data-page");
  if (page === "accounts") initAccounts();
  else if (page === "tokens") initTokens();
  else if (page === "settings") initSettings();
})();
