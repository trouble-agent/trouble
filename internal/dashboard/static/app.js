/* trouble dashboard // SPEC-10 §2.6
 *
 * The htmx-loaded poller. It does four things and nothing else:
 *   1. copies the <meta name="trouble-csrf"> value into X-Trouble-CSRF on
 *      every htmx request (§2.3 — the double-submit pair);
 *   2. dispatches the `troubleSeq` DOM event when #health-strip's data-seq
 *      changes, which is the accelerator the content partials listen to
 *      via hx-trigger="every 2s, troubleSeq from:body";
 *   3. suspends polling while the tab is hidden for ≥10s and forces one
 *      refresh on visibilitychange;
 *   4. drives the #stale-banner: three consecutive identical strip seqs AND
 *      data-stall-s ≥ stall_alert_s, any 429/5xx poll, or no successful poll
 *      for 5× the poll interval.
 *
 * The banner is advisory — the authoritative stall alarm is SPEC-12's external
 * checker on /health.json. No inline script anywhere: this file is the only JS
 * the dashboard ships, alongside vendored htmx.
 */
(function () {
  "use strict";

  var HIDE_SUSPEND_MS = 10000;

  function meta(name, fallback) {
    var el = document.querySelector('meta[name="' + name + '"]');
    var v = el ? el.getAttribute("content") : "";
    return v === "" || v === null ? fallback : v;
  }

  var csrf = meta("trouble-csrf", "");
  var pollMS = parseInt(meta("trouble-poll-ms", "2000"), 10) || 2000;
  var stripMS = parseInt(meta("trouble-strip-poll-ms", "1000"), 10) || 1000;
  var stallAlertS = parseFloat(meta("trouble-stall-alert-s", "90")) || 90;

  var lastSeq = null;
  var repeat = 0;
  var lastOKAt = Date.now();
  var bannerOn = false;
  var suspended = false;
  var hiddenAt = null;

  function showBanner(on) {
    if (on === bannerOn) {
      return;
    }
    bannerOn = on;
    var b = document.getElementById("stale-banner");
    if (!b) {
      return;
    }
    b.hidden = !on;
    b.setAttribute("data-stalled", on ? "true" : "false");
  }

  /* 1. the double-submit pair on every htmx request */
  document.body.addEventListener("htmx:configRequest", function (evt) {
    if (csrf) {
      evt.detail.headers["X-Trouble-CSRF"] = csrf;
    }
  });

  /* 2 + 4. sequence tracking and the stale banner inputs */
  function trackStrip(strip) {
    lastOKAt = Date.now();
    var seq = strip.getAttribute("data-seq") || "";
    var stall = parseFloat(strip.getAttribute("data-stall-s") || "0");
    var changed = lastSeq !== null && seq !== lastSeq;
    if (seq === lastSeq) {
      repeat += 1;
    } else {
      repeat = 0;
    }
    lastSeq = seq;

    if (strip.getAttribute("data-banner") === "true" || (repeat >= 2 && stall >= stallAlertS)) {
      showBanner(true);
    } else if (stall < stallAlertS) {
      showBanner(false);
    }
    if (changed) {
      document.body.dispatchEvent(new CustomEvent("troubleSeq", { detail: { seq: seq } }));
    }
  }

  document.body.addEventListener("htmx:afterRequest", function (evt) {
    var xhr = evt.detail.xhr;
    if (xhr && (xhr.status === 429 || xhr.status >= 500)) {
      showBanner(true); /* (b) any 429/5xx poll */
      return;
    }
    if (xhr && xhr.status >= 200 && xhr.status < 300) {
      lastOKAt = Date.now();
    }
    var el = evt.detail.elt;
    if (el && el.id === "health-strip") {
      trackStrip(el);
    }
  });

  /* 3. suspending: skip poll requests while hidden and suspended */
  document.body.addEventListener("htmx:beforeRequest", function (evt) {
    var el = evt.detail.elt;
    if (!suspended || !el || !el.getAttribute) {
      return;
    }
    var trigger = el.getAttribute("hx-trigger") || "";
    if (trigger.indexOf("every") !== -1) {
      evt.preventDefault();
    }
  });

  window.setInterval(function () {
    if (document.visibilityState === "hidden") {
      if (hiddenAt === null) {
        hiddenAt = Date.now();
      }
      if (!suspended && Date.now() - hiddenAt >= HIDE_SUSPEND_MS) {
        suspended = true; /* a phone in a pocket must not burn tailnet traffic */
      }
      return;
    }
    hiddenAt = null;
    var interval = Math.min(pollMS, stripMS);
    if (!suspended && Date.now() - lastOKAt > 5 * interval) {
      showBanner(true); /* (c) no successful poll for 5× the interval */
    }
  }, 1000);

  /* 3 (cont.). one forced refresh on visibilitychange */
  document.addEventListener("visibilitychange", function () {
    if (document.visibilityState === "hidden") {
      hiddenAt = Date.now();
      return;
    }
    var hiddenFor = hiddenAt === null ? 0 : Date.now() - hiddenAt;
    hiddenAt = null;
    suspended = false;
    lastOKAt = Date.now();
    document.body.dispatchEvent(new CustomEvent("troubleSeq", { detail: { seq: lastSeq, forced: true } }));
    if (hiddenFor >= HIDE_SUSPEND_MS && window.htmx) {
      htmx.ajax("GET", "/partials/health", { target: "#health-strip", swap: "outerHTML" });
    }
  });
})();