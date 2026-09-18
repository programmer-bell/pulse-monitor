(function () {
  var indicator = document.getElementById("live-indicator");
  var label = indicator.querySelector(".live-label");
  var tbody = document.getElementById("targets-tbody");
  var empty = document.getElementById("empty-row");

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

  document.body.addEventListener("htmx:sseOpen", function () { setLive(true); });
  document.body.addEventListener("htmx:sseError", function () { setLive(false); });
  document.body.addEventListener("htmx:sseClose", function () { setLive(false); });

  new MutationObserver(syncEmpty).observe(tbody, { childList: true });
  syncEmpty();
})();