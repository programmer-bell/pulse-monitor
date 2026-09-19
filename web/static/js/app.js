(function () {
  var indicator = document.getElementById("live-indicator");
  var label = indicator.querySelector(".live-label");
  var tbody = document.getElementById("targets-tbody");
  var empty = document.getElementById("empty-row");
  var toast = document.getElementById("toast");
  var toastTimer = null;

  var PLACEHOLDER = "—";

  function setLive(on) {
    indicator.classList.toggle("is-live", on);
    indicator.classList.toggle("is-error", !on);
    label.textContent = on ? "Live" : "Reconnecting";
  }

  function hasTargets() {
    return tbody.querySelector('tr[id^="target-"]') !== null;
  }

  function syncEmpty() {
    empty.hidden = hasTargets();
  }

  function showToast(message, type) {
    toast.textContent = message;
    toast.className = "toast" + (type ? " toast-" + type : "");
    toast.hidden = false;
    clearTimeout(toastTimer);
    toastTimer = setTimeout(function () { toast.hidden = true; }, 4000);
  }

  function setText(id, text) {
    var el = document.getElementById(id);
    if (el) el.textContent = text;
  }

  document.body.addEventListener("htmx:sseOpen", function () { setLive(true); });
  document.body.addEventListener("htmx:sseError", function () { setLive(false); });
  document.body.addEventListener("htmx:sseClose", function () { setLive(false); });

  new MutationObserver(syncEmpty).observe(tbody, { childList: true });
  syncEmpty();

  // --- add-target form: loading state + error feedback ---
  var addForm = document.querySelector(".add-target form");
  var addBtn = addForm.querySelector("button[type=submit]");

  addForm.addEventListener("htmx:beforeRequest", function () {
    addBtn.disabled = true;
    addBtn.classList.add("is-loading");
  });
  addForm.addEventListener("htmx:afterRequest", function () {
    addBtn.disabled = false;
    addBtn.classList.remove("is-loading");
  });
  addForm.addEventListener("htmx:responseError", function (evt) {
    var msg = (evt.detail.xhr && evt.detail.xhr.responseText) || "Request failed";
    showToast(msg.trim(), "error");
  });
  addForm.addEventListener("htmx:sendError", function () {
    showToast("Network error — could not reach the server", "error");
  });

  // --- metrics (GET /metrics) ---
  function fmtDuration(seconds) {
    var s = Math.max(0, Math.floor(seconds));
    var d = Math.floor(s / 86400);
    var h = Math.floor((s % 86400) / 3600);
    var m = Math.floor((s % 3600) / 60);
    var rest = s % 60;
    if (d > 0) return d + "d " + h + "h";
    if (h > 0) return h + "h " + m + "m";
    if (m > 0) return m + "m " + rest + "s";
    return rest + "s";
  }

  function renderMetrics(data) {
    var total = data.checks_total != null ? parseInt(data.checks_total, 10) : 0;
    var failed = data.checks_failures != null ? parseInt(data.checks_failures, 10) : 0;
    setText("m-checks", total.toLocaleString());
    setText("m-inflight", data.checks_in_flight != null ? parseInt(data.checks_in_flight, 10) : PLACEHOLDER);
    setText("m-failures", failed.toLocaleString());

    var pct = total > 0 ? ((total - failed) / total) * 100 : null;
    setText("m-success", pct != null ? pct.toFixed(1) + "%" : PLACEHOLDER);
    var bar = document.getElementById("m-success-bar");
    if (bar) bar.style.width = (pct != null ? Math.min(100, pct) : 0).toFixed(1) + "%";

    setText("m-uptime", data.uptime_seconds != null ? fmtDuration(parseFloat(data.uptime_seconds)) : PLACEHOLDER);

    var failEl = document.getElementById("m-failures");
    if (failEl) failEl.classList.toggle("is-alert", failed > 0);
  }

  function clearMetrics() {
    setText("m-checks", PLACEHOLDER);
    setText("m-inflight", PLACEHOLDER);
    setText("m-failures", PLACEHOLDER);
    setText("m-success", PLACEHOLDER);
    setText("m-uptime", PLACEHOLDER);
    var bar = document.getElementById("m-success-bar");
    if (bar) bar.style.width = "0%";
  }

  var polling = false;
  function pollMetrics() {
    if (polling) return;
    polling = true;
    fetch("/metrics", { cache: "no-store" })
      .then(function (res) {
        if (!res.ok) throw new Error("metrics request failed");
        return res.text();
      })
      .then(function (text) {
        var data = {};
        text.split("\n").forEach(function (line) {
          var i = line.indexOf(" ");
          if (i < 0) return;
          data[line.slice(0, i)] = line.slice(i + 1);
        });
        renderMetrics(data);
      })
      .catch(function () {
        clearMetrics();
      })
      .finally(function () {
        polling = false;
      });
  }

  pollMetrics();
  setInterval(pollMetrics, 5000);
})();