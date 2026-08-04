var activeModelInput = null;

document.addEventListener("click", function (event) {
  var copy = event.target.closest("[data-copy]");
  if (copy) {
    var node = document.querySelector(copy.getAttribute("data-copy"));
    if (node) {
      var value = node.textContent.trim();
      navigator.clipboard.writeText(value).catch(function () {});
      copy.classList.add("is-copied");
      setTimeout(function () { copy.classList.remove("is-copied"); }, 1000);
    }
    return;
  }

  var testBtn = event.target.closest("[data-test-model]");
  if (testBtn) {
    event.preventDefault();
    var modelId = testBtn.getAttribute("data-test-model") || "";
    var chip = testBtn.closest(".model-chip");
    var status = chip ? chip.querySelector(".model-test-status") : null;
    testModel(modelId, testBtn, status);
    return;
  }
  var testAdd = event.target.closest("[data-test-add-model]");
  if (testAdd) {
    event.preventDefault();
    var form = document.getElementById("add-model-form");
    var input = form ? form.querySelector('input[name="id"]') : null;
    var statusAdd = document.getElementById("add-model-test-status");
    testModel(input ? input.value.trim() : "", testAdd, statusAdd);
    return;
  }

  var openAdd = event.target.closest("[data-open-add-model]");
  if (openAdd) {
    var form = document.getElementById("add-model-form");
    if (form) {
      form.hidden = false;
      var input = form.querySelector('input[name="id"]');
      if (input) input.focus();
    }
    return;
  }
  var closeAdd = event.target.closest("[data-close-add-model]");
  if (closeAdd) {
    var addForm = document.getElementById("add-model-form");
    if (addForm) addForm.hidden = true;
    return;
  }

  var openPicker = event.target.closest("[data-open-model-picker]");
  if (openPicker) {
    openModelPicker(openPicker.getAttribute("data-open-model-picker"), openPicker.getAttribute("data-picker-title") || "Select model");
    return;
  }
  var pick = event.target.closest(".pick-chip");
  if (pick && pick.closest("#model-picker-list")) {
    applyModelPick(pick.getAttribute("data-model-id") || "");
    return;
  }
  if (event.target && event.target.id === "model-picker-use-custom") {
    var custom = document.getElementById("model-picker-custom");
    applyModelPick(custom ? custom.value.trim() : "");
    return;
  }

  var link = event.target.closest("a[data-oauth-popup]");
  if (link) {
    var popup = window.open(link.href, link.target || "literouter_oauth", "popup=yes,width=560,height=760,resizable=yes,scrollbars=yes");
    if (popup) {
      popup.focus();
      event.preventDefault();
      watchOAuthPopup(popup);
    }
    return;
  }
  var toggle = event.target.closest("[data-manual-toggle]");
  if (toggle) {
    var panel = document.getElementById(toggle.dataset.manualToggle);
    if (panel) panel.hidden = !panel.hidden;
  }
});

function openModelPicker(inputKey, title) {
  activeModelInput = document.querySelector('[data-model-input="' + inputKey + '"]');
  var dialog = document.getElementById("model-picker");
  var list = document.getElementById("model-picker-list");
  var source = document.getElementById("model-picker-source");
  var titleEl = document.getElementById("model-picker-title");
  var search = document.getElementById("model-picker-search");
  var custom = document.getElementById("model-picker-custom");
  if (!dialog || !list || !source) return;
  if (titleEl) titleEl.textContent = title || "Select model";
  list.innerHTML = source.innerHTML;
  var isList = !!(activeModelInput && activeModelInput.hasAttribute("data-model-list"));
  var hint = document.getElementById("model-picker-hint");
  if (hint) {
    hint.textContent = isList
      ? "Click chips to add or remove them. Selected ones stay highlighted; close when done."
      : "Click a chip to fill the field. You can still type any free-text id.";
  }
  if (search) search.value = "";
  // A list field must not preload the whole list into the custom box, where "Use custom"
  // would then re-add the entire string as one entry.
  if (custom) custom.value = isList || !activeModelInput ? "" : activeModelInput.value;
  filterModelPicker("");
  if (typeof dialog.showModal === "function") dialog.showModal();
  if (search) search.focus();
}

function filterModelPicker(query) {
  var list = document.getElementById("model-picker-list");
  if (!list) return;
  var q = (query || "").toLowerCase();
  list.querySelectorAll(".pick-chip").forEach(function (chip) {
    var id = chip.getAttribute("data-model-id") || "";
    var label = (chip.getAttribute("data-model-label") || "").toLowerCase();
    var provider = (chip.getAttribute("data-provider") || "").toLowerCase();
    chip.hidden = !!(q && id.toLowerCase().indexOf(q) === -1 && label.indexOf(q) === -1 && provider.indexOf(q) === -1);
    var selected = false;
    if (activeModelInput) {
      selected = activeModelInput.hasAttribute("data-model-list")
        ? modelListValues(activeModelInput).indexOf(id) !== -1
        : id === activeModelInput.value;
    }
    chip.classList.toggle("is-active", selected);
  });
  list.querySelectorAll(".pick-group").forEach(function (group) {
    var visible = group.querySelector(".pick-chip:not([hidden])");
    group.hidden = !visible;
  });
}

// modelListValues splits a comma-separated field into its entries.
function modelListValues(input) {
  if (!input) return [];
  return input.value.split(",").map(function (part) { return part.trim(); })
    .filter(function (part) { return part !== ""; });
}

function applyModelPick(value) {
  if (!activeModelInput || !value) return;
  // A list field accumulates instead of being overwritten, and the dialog stays open so a
  // whole set can be picked in one visit — which is the point of browsing it at all. A
  // second click on the same chip removes it, so a mistake does not mean editing the text
  // by hand.
  if (activeModelInput.hasAttribute("data-model-list")) {
    var values = modelListValues(activeModelInput);
    var at = values.indexOf(value);
    if (at === -1) {
      values.push(value);
    } else {
      values.splice(at, 1);
    }
    activeModelInput.value = values.join(", ");
    activeModelInput.dispatchEvent(new Event("input", {bubbles: true}));
    var search = document.getElementById("model-picker-search");
    filterModelPicker(search ? search.value : "");
    return;
  }
  activeModelInput.value = value;
  activeModelInput.dispatchEvent(new Event("input", {bubbles: true}));
  var dialog = document.getElementById("model-picker");
  if (dialog && dialog.open) dialog.close();
  activeModelInput.focus();
}

document.addEventListener("input", function (event) {
  if (event.target && event.target.id === "model-picker-search") {
    filterModelPicker(event.target.value);
  }
});
document.addEventListener("submit", async function (event) {
  var form = event.target.closest("form[data-download-form], form[data-cli-setup-form]");
  if (!form) return;
  event.preventDefault();
  var button = event.submitter;
  var action = button && button.formAction ? button.formAction : form.action;
  var download = button && button.getAttribute("data-download") === "1";
  if (button) button.disabled = true;
  var flash = form.querySelector(".cli-setup-result") || ensureCliFlash(form);
  flash.className = "cli-setup-result";
  flash.textContent = download ? "Preparing script…" : "Applying…";
  try {
    var url = action;
    if (download && url.indexOf("download=1") === -1) {
      url += (url.indexOf("?") >= 0 ? "&" : "?") + "download=1";
    }
    var response = await fetch(url, {method: "POST", body: new FormData(form), credentials: "same-origin"});
    var contentType = response.headers.get("Content-Type") || "";
    if (!response.ok) {
      var errText = await response.text();
      // strip html tags lightly
      errText = errText.replace(/<[^>]+>/g, " ").replace(/\s+/g, " ").trim();
      throw new Error(errText || "Request failed");
    }
    if (download || contentType.indexOf("shellscript") >= 0 || (response.headers.get("Content-Disposition") || "").indexOf("attachment") >= 0) {
      var disposition = response.headers.get("Content-Disposition") || "";
      var match = disposition.match(/filename="([^"]+)"/);
      var anchor = document.createElement("a");
      anchor.href = URL.createObjectURL(await response.blob());
      anchor.download = match ? match[1] : "literouter-setup.sh";
      anchor.click();
      URL.revokeObjectURL(anchor.href);
      flash.className = "cli-setup-result ok";
      flash.textContent = "Script downloaded";
    } else {
      var html = await response.text();
      flash.className = "cli-setup-result ok";
      flash.innerHTML = html || "Done";
    }
  } catch (error) {
    flash.className = "cli-setup-result bad";
    flash.textContent = error.message || "Failed";
  } finally {
    if (button) button.disabled = false;
  }
});

function ensureCliFlash(form) {
  var el = document.createElement("div");
  el.className = "cli-setup-result";
  form.appendChild(el);
  return el;
}

window.addEventListener("message", function (event) {
  var data = event.data || {};
  if (data.type !== "literouter-oauth" || !data.ok) return;
  clearOAuthWaiting();
  refreshAccounts();
});

document.body.addEventListener("htmx:afterOnLoad", function (event) {
  var path = event.detail && event.detail.pathInfo && event.detail.pathInfo.requestPath;
  if (!path || path.indexOf("/ui/tabs/") !== 0) return;
  var parts = path.split("/").filter(Boolean); // ui, tabs, ...
  var tab = parts[parts.length - 1] || "endpoint";
  var parent = parts[parts.length - 2];
  var titles = {
    endpoint: ["Endpoint & Key", "Local gateway endpoints"],
    providers: ["Providers", "OAuth providers & pool"],
    quota: ["Quota Tracker", "Session & weekly limits"],
    usage: ["Usage & Analytics", "Traffic, cost & routing"],
    cli: ["CLI Tools", "Apply host client configs"],
    codex: ["Providers", "Codex"],
    claude: ["Providers", "Claude"],
    xai: ["Providers", "xAI (Grok)"],
    grok: ["Providers", "xAI (Grok)"]
  };
  var activeNav = parent === "providers" && titles[tab] ? "providers" : tab;
  var meta = titles[tab] || titles.endpoint;
  var eyebrow = document.querySelector(".page-kicker");
  var heading = document.querySelector(".page-title");
  if (eyebrow) eyebrow.textContent = meta[0];
  if (heading) heading.textContent = meta[1];
  document.querySelectorAll(".nav-item").forEach(function (link) {
    link.classList.toggle("is-active", link.getAttribute("data-tab") === activeNav);
  });
});

function currentProviderDetail() {
  return document.querySelector('.provider-detail[data-provider]');
}

function accountsURL() {
  var detail = currentProviderDetail();
  if (detail) {
    var provider = detail.getAttribute("data-provider") || "";
    if (provider) {
      return "/ui/accounts?provider=" + encodeURIComponent(provider) + "&view=connections";
    }
  }
  return "/ui/accounts";
}

function refreshAccounts() {
  if (!window.htmx) return;
  if (document.querySelector("#accounts")) {
    window.htmx.ajax("GET", accountsURL(), {target: "#accounts", swap: "innerHTML"});
  }
  if (document.querySelector("#quota-accounts")) {
    window.htmx.ajax("GET", quotaBoardURL(), {target: "#quota-accounts", swap: "innerHTML"});
  }
  // Refresh provider detail chrome (active counts) without full navigation when possible.
  var detail = currentProviderDetail();
  if (detail && window.htmx) {
    var provider = detail.getAttribute("data-provider") || "";
    if (provider) {
      // Soft-refresh connection summary text via full tab partial would be heavy;
      // counts update on next navigation. Keep list correct first.
    }
  }
}

function clearOAuthWaiting() {
  var box = document.getElementById("oauth-result");
  if (box) box.innerHTML = "";
}

function watchOAuthPopup(popup) {
  var started = Date.now();
  var timer = setInterval(function () {
    if (popup.closed || Date.now() - started > 120000) {
      clearInterval(timer);
      clearOAuthWaiting();
      refreshAccounts();
      return;
    }
    refreshAccounts();
  }, 2000);
}

setInterval(function () {
  if (document.visibilityState !== "visible") return;
  // Only auto-refresh while a provider detail page is open.
  var detail = currentProviderDetail();
  if (!detail || detail.getAttribute("data-auto-refresh") !== "connections") return;
  if (!window.htmx || !document.querySelector("#accounts")) return;
  window.htmx.ajax("GET", accountsURL(), {
    target: "#accounts",
    swap: "innerHTML"
  });
}, 60000);


function testModel(modelId, button, statusEl) {
  if (!modelId) {
    if (statusEl) {
      statusEl.hidden = false;
      statusEl.className = "model-test-status is-bad";
      statusEl.textContent = "Enter a model id first";
    }
    return;
  }
  if (button) button.disabled = true;
  if (button) button.classList.add("is-loading");
  if (statusEl) {
    statusEl.hidden = false;
    statusEl.className = "model-test-status is-pending";
    statusEl.textContent = "Testing…";
  }
  var body = new URLSearchParams();
  body.set("id", modelId);
  fetch("/ui/models/test?format=json", {
    method: "POST",
    headers: {
      "Content-Type": "application/x-www-form-urlencoded",
      "Accept": "application/json"
    },
    body: body.toString(),
    credentials: "same-origin"
  }).then(function (res) {
    return res.text().then(function (text) {
      var data = {};
      try { data = text ? JSON.parse(text) : {}; } catch (e) { data = { error: text || res.statusText }; }
      return { status: res.status, data: data };
    });
  }).then(function (result) {
    var data = result.data || {};
    if (!statusEl) return;
    statusEl.hidden = false;
    if (data.ok) {
      statusEl.className = "model-test-status is-ok";
      statusEl.textContent = "OK · " + (data.latency || "?") + " · " + (data.preview || "pong");
      var chip = button && button.closest(".model-chip");
      if (chip) {
        chip.classList.add("is-tested-ok");
        chip.classList.remove("is-tested-bad");
      }
      return;
    }
    var err = data.error || data.message || ("HTTP " + result.status);
    if (result.status === 401 || /unauthor/i.test(String(err))) {
      err = "Unauthorized — check LITEROUTER_API_TOKEN for API clients";
    }
    statusEl.className = "model-test-status is-bad";
    statusEl.textContent = "Failed · " + err;
    var chipBad = button && button.closest(".model-chip");
    if (chipBad) {
      chipBad.classList.remove("is-tested-ok");
      chipBad.classList.add("is-tested-bad");
    }
  }).catch(function (err) {
    if (!statusEl) return;
    statusEl.hidden = false;
    statusEl.className = "model-test-status is-bad";
    statusEl.textContent = "Failed · " + (err && err.message ? err.message : "network error");
  }).finally(function () {
    if (button) {
      button.disabled = false;
      button.classList.remove("is-loading");
    }
  });
}


document.body.addEventListener("htmx:afterSwap", function (event) {
  if (!event.detail || event.detail.target && event.detail.target.id !== "accounts") {
    // still try if target is accounts
  }
  var target = event.detail && event.detail.target;
  if (!target || target.id !== "accounts") return;
  var rows = target.querySelectorAll(".conn-row");
  if (!rows.length && !target.querySelector(".conn-list") && !target.querySelector(".empty-state")) return;
  var total = target.querySelectorAll(".conn-row").length;
  var active = 0;
  var exhausted = 0;
  target.querySelectorAll(".conn-row").forEach(function (row) {
    if (row.querySelector(".pill.ok")) active++;
    if (row.querySelector(".pill.danger")) exhausted++;
  });
  var head = document.querySelector(".connections-head p");
  if (head) {
    head.textContent = active + " active · " + exhausted + " exhausted · auto-health every 1m";
  }
  var tag = document.querySelector(".provider-hero-title .tag");
  if (tag) {
    tag.textContent = total + " connection" + (total === 1 ? "" : "s");
    tag.classList.toggle("ok", active > 0);
  }
});


function quotaBoardEl() {
  return document.querySelector("#quota-board") || document.querySelector("#quota-accounts .quota-board");
}

function quotaBoardURL() {
  var board = quotaBoardEl();
  var params = new URLSearchParams();
  params.set("view", "quota");
  var provider = "", status = "", sort = "expiring", auto = "1";
  if (board) {
    var p = board.querySelector('[data-quota-filter="provider"]');
    var s = board.querySelector('[data-quota-filter="status"]');
    var o = board.querySelector('[data-quota-filter="sort"]');
    if (p) provider = p.value || "";
    if (s) status = s.value || "";
    if (o) sort = o.value || "expiring";
    auto = board.getAttribute("data-auto-refresh") === "1" ? "1" : "0";
  }
  if (provider) params.set("provider", provider);
  if (status) params.set("status", status);
  if (sort) params.set("sort", sort);
  params.set("auto", auto);
  return "/ui/accounts?" + params.toString();
}

function refreshQuotaBoard() {
  if (!window.htmx || !document.querySelector("#quota-accounts")) return;
  window.htmx.ajax("GET", quotaBoardURL(), {target: "#quota-accounts", swap: "innerHTML"});
}

var quotaTimer = null;
var quotaDeadline = 0;

function stopQuotaAutoRefresh() {
  if (quotaTimer) {
    clearInterval(quotaTimer);
    quotaTimer = null;
  }
}

function startQuotaAutoRefresh() {
  stopQuotaAutoRefresh();
  var board = quotaBoardEl();
  if (!board || board.getAttribute("data-auto-refresh") !== "1") return;
  if (document.visibilityState !== "visible") return;
  quotaDeadline = Date.now() + 60000;
  updateQuotaCountdown();
  quotaTimer = setInterval(function () {
    if (document.visibilityState !== "visible") return;
    var left = Math.max(0, Math.ceil((quotaDeadline - Date.now()) / 1000));
    updateQuotaCountdown(left);
    if (left <= 0) {
      quotaDeadline = Date.now() + 60000;
      refreshQuotaBoard();
    }
  }, 1000);
}

function updateQuotaCountdown(left) {
  var el = document.querySelector("[data-quota-countdown]");
  if (!el) return;
  if (typeof left !== "number") {
    left = Math.max(0, Math.ceil((quotaDeadline - Date.now()) / 1000));
  }
  var board = quotaBoardEl();
  if (!board || board.getAttribute("data-auto-refresh") !== "1") {
    el.textContent = "Auto-refresh";
    return;
  }
  el.textContent = "Auto-refresh (" + left + "s)";
}

document.addEventListener("change", function (event) {
  var filter = event.target && event.target.getAttribute("data-quota-filter");
  if (!filter) return;
  refreshQuotaBoard();
});

function quotaGroupKey(provider) {
  return "literouter.quota.collapsed." + provider;
}

function restoreQuotaGroups() {
  document.querySelectorAll("[data-quota-provider]").forEach(function (group) {
    var provider = group.getAttribute("data-quota-provider") || "";
    var collapsed = false;
    try { collapsed = localStorage.getItem(quotaGroupKey(provider)) === "1"; } catch (e) {}
    group.classList.toggle("is-collapsed", collapsed);
    var toggle = group.querySelector("[data-quota-group-toggle]");
    if (toggle) toggle.setAttribute("aria-expanded", collapsed ? "false" : "true");
  });
}

document.addEventListener("click", function (event) {
  var groupToggle = event.target.closest("[data-quota-group-toggle]");
  if (groupToggle) {
    event.preventDefault();
    var group = groupToggle.closest("[data-quota-provider]");
    if (!group) return;
    var collapsed = group.classList.toggle("is-collapsed");
    groupToggle.setAttribute("aria-expanded", collapsed ? "false" : "true");
    try { localStorage.setItem(quotaGroupKey(group.getAttribute("data-quota-provider") || ""), collapsed ? "1" : "0"); } catch (e) {}
    return;
  }
  var refreshBtn = event.target.closest("[data-quota-refresh]");
  if (refreshBtn) {
    event.preventDefault();
    refreshQuotaBoard();
    return;
  }
  var autoBtn = event.target.closest("[data-quota-autorefresh]");
  if (autoBtn) {
    event.preventDefault();
    var board = quotaBoardEl();
    if (!board) return;
    var on = board.getAttribute("data-auto-refresh") !== "1";
    board.setAttribute("data-auto-refresh", on ? "1" : "0");
    autoBtn.classList.toggle("is-on", on);
    if (on) startQuotaAutoRefresh();
    else {
      stopQuotaAutoRefresh();
      updateQuotaCountdown();
    }
    return;
  }
  var bulk = event.target.closest("[data-quota-bulk]");
  if (bulk) {
    event.preventDefault();
    if (!window.htmx) return;
    var board = quotaBoardEl();
    var action = bulk.getAttribute("data-quota-bulk");
    var body = new URLSearchParams();
    body.set("action", action);
    if (board) {
      var p = board.querySelector('[data-quota-filter="provider"]');
      var s = board.querySelector('[data-quota-filter="status"]');
      var o = board.querySelector('[data-quota-filter="sort"]');
      if (p && p.value) body.set("provider", p.value);
      if (s && s.value) body.set("status", s.value);
      if (o && o.value) body.set("sort", o.value);
      body.set("auto", board.getAttribute("data-auto-refresh") === "1" ? "1" : "0");
    }
    window.htmx.ajax("POST", "/ui/accounts/bulk", {
      target: "#quota-accounts",
      swap: "innerHTML",
      values: Object.fromEntries(body.entries())
    });
  }
});

document.body.addEventListener("htmx:afterSwap", function (event) {
  var target = event.detail && event.detail.target;
  if (target && (target.id === "quota-accounts" || target.id === "content")) {
    if (quotaBoardEl()) {
      restoreQuotaGroups();
      startQuotaAutoRefresh();
    } else stopQuotaAutoRefresh();
  }
});

document.addEventListener("visibilitychange", function () {
  if (document.visibilityState === "visible" && quotaBoardEl()) startQuotaAutoRefresh();
  else if (document.visibilityState !== "visible") stopQuotaAutoRefresh();
});

// boot if quota page already rendered
if (document.readyState === "loading") {
  document.addEventListener("DOMContentLoaded", function () {
    if (quotaBoardEl()) {
      restoreQuotaGroups();
      startQuotaAutoRefresh();
    }
  });
} else if (quotaBoardEl()) {
  restoreQuotaGroups();
  startQuotaAutoRefresh();
}


function mountOAuthModal(root) {
  var modal = root && root.querySelector ? root.querySelector("[data-oauth-modal]") : null;
  if (!modal && root && root.hasAttribute && root.hasAttribute("data-oauth-modal")) modal = root;
  if (!modal) return;
  // Move to body so it overlays full page.
  if (modal.parentElement !== document.body) {
    document.body.appendChild(modal);
  }
  document.documentElement.classList.add("oauth-open");
  var authURL = modal.getAttribute("data-auth-url") || "";
  if (authURL) {
    var popup = window.open(authURL, "literouter_oauth", "popup=yes,width=560,height=760,resizable=yes,scrollbars=yes");
    if (popup) {
      try { popup.focus(); } catch (e) {}
      watchOAuthPopup(popup);
    }
  }
}

function closeOAuthModal() {
  document.querySelectorAll("[data-oauth-modal]").forEach(function (el) { el.remove(); });
  document.documentElement.classList.remove("oauth-open");
  var box = document.getElementById("oauth-result");
  if (box) box.innerHTML = "";
}

document.addEventListener("click", function (event) {
  if (event.target.closest("[data-oauth-close]")) {
    event.preventDefault();
    closeOAuthModal();
    return;
  }
  var modal = event.target.closest("[data-oauth-modal]");
  if (modal && event.target === modal) {
    closeOAuthModal();
  }
});

document.addEventListener("keydown", function (event) {
  if (event.key === "Escape" && document.querySelector("[data-oauth-modal]")) {
    closeOAuthModal();
  }
});

document.body.addEventListener("htmx:afterSwap", function (event) {
  var target = event.detail && event.detail.target;
  if (!target) return;
  if (target.id === "oauth-result" || target.querySelector && target.querySelector("[data-oauth-modal]")) {
    mountOAuthModal(target);
  }
  if (target.id === "oauth-complete-result" && target.querySelector("[data-oauth-done]")) {
    refreshAccounts();
    setTimeout(closeOAuthModal, 700);
  }
});

// When popup auth succeeds via postMessage, close modal too.
window.addEventListener("message", function (event) {
  var data = event.data || {};
  if (data.type === "literouter-oauth" && data.ok) {
    closeOAuthModal();
  }
});

function usagePageEl() {
  return document.getElementById("usage-page") || document.querySelector("[data-usage-page]");
}

function usageRefreshURL() {
  var page = usagePageEl();
  var range = (page && page.getAttribute("data-range")) || "24h";
  var active = document.querySelector(".range-pill.is-active");
  if (active) {
    try {
      var u = new URL(active.getAttribute("href") || "", window.location.origin);
      var r = u.searchParams.get("range");
      if (r) range = r;
    } catch (e) {}
  }
  return "/ui/tabs/usage?range=" + encodeURIComponent(range);
}

var usageTimer = null;
var usageDeadline = 0;

function stopUsageAutoRefresh() {
  if (usageTimer) {
    clearInterval(usageTimer);
    usageTimer = null;
  }
}

function refreshUsagePage() {
  if (!window.htmx || !usagePageEl()) return;
  if (document.visibilityState !== "visible") return;
  var content = document.getElementById("content");
  if (!content) return;
  window.htmx.ajax("GET", usageRefreshURL(), {
    target: "#content",
    swap: "innerHTML settle:80ms"
  });
}

function updateUsageCountdown(left) {
  var el = document.querySelector("[data-usage-countdown]");
  if (!el) return;
  if (!usagePageEl()) {
    el.textContent = "Auto 30s";
    return;
  }
  if (typeof left !== "number") {
    left = Math.max(0, Math.ceil((usageDeadline - Date.now()) / 1000));
  }
  el.textContent = "Auto " + left + "s";
}

function startUsageAutoRefresh() {
  stopUsageAutoRefresh();
  var page = usagePageEl();
  if (!page || page.getAttribute("data-auto-refresh") !== "1") return;
  if (document.visibilityState !== "visible") return;
  usageDeadline = Date.now() + 30000;
  updateUsageCountdown();
  usageTimer = setInterval(function () {
    if (document.visibilityState !== "visible") return;
    if (!usagePageEl()) {
      stopUsageAutoRefresh();
      return;
    }
    var left = Math.max(0, Math.ceil((usageDeadline - Date.now()) / 1000));
    updateUsageCountdown(left);
    if (left <= 0) {
      usageDeadline = Date.now() + 30000;
      refreshUsagePage();
    }
  }, 1000);
}

document.body.addEventListener("htmx:afterSwap", function (event) {
  var target = event.detail && event.detail.target;
  if (target && (target.id === "content" || target.id === "usage-page")) {
    if (usagePageEl()) startUsageAutoRefresh();
    else stopUsageAutoRefresh();
  }
});

document.addEventListener("visibilitychange", function () {
  if (document.visibilityState === "visible" && usagePageEl()) startUsageAutoRefresh();
  else if (document.visibilityState !== "visible") stopUsageAutoRefresh();
});

if (document.readyState === "loading") {
  document.addEventListener("DOMContentLoaded", function () {
    if (usagePageEl()) startUsageAutoRefresh();
  });
} else if (usagePageEl()) {
  startUsageAutoRefresh();
}




// Validate Grok/Codex OAuth manual paste before submit so Connect never "does nothing".
document.body.addEventListener("submit", function (event) {
  var form = event.target && event.target.closest ? event.target.closest("[data-oauth-complete-form]") : null;
  if (!form) return;
  var input = form.querySelector("[data-oauth-callback-input]");
  var out = form.querySelector("#oauth-complete-result") || form.querySelector(".oauth-complete-result");
  if (!input) return;
  var raw = (input.value || "").trim();
  var looksURL = /https?:\/\//i.test(raw) || raw.indexOf("callback?") >= 0 || raw.indexOf("code=") >= 0 || raw.indexOf("state=") >= 0 || raw.indexOf("127.0.0.1") >= 0 || raw.indexOf("localhost") >= 0;
  var looksBareCode = !!raw && !/\s/.test(raw) && raw.indexOf("/") < 0 && raw.length >= 16;
  if (!raw || (!looksURL && !looksBareCode)) {
    event.preventDefault();
    event.stopPropagation();
    if (out) {
      out.innerHTML = '<div class="oauth-modal-error" role="alert"><strong>Connect failed</strong><p>Paste the callback URL or the bare authorization code.</p><p class="oauth-error-hint">Best: <code>http://127.0.0.1:1456/callback?code=...&amp;state=...</code>. Or paste only the <code>code</code> value right after opening Connect.</p></div>';
    }
    input.focus();
    input.select();
  } else if (looksURL && raw.indexOf("code=") < 0) {
    event.preventDefault();
    event.stopPropagation();
    if (out) {
      out.innerHTML = '<div class="oauth-modal-error" role="alert"><strong>Connect failed</strong><p>That URL has no <code>code=</code>. Copy the address bar <em>after</em> Grok redirects to 127.0.0.1:1456/callback.</p></div>';
    }
    input.focus();
    input.select();
  }
}, true);

// Account switches submit their form on change.
//
// Bound here rather than with an inline onchange, because the dashboard is served with
// `Content-Security-Policy: script-src 'self'` and that blocks inline handlers outright. The
// failure was silent and looked exactly like a broken toggle: clicking flipped the checkbox,
// because that is native browser behaviour needing no JavaScript, while the handler never ran
// and no request was ever sent. The server was never asked to change anything, so a reload
// showed the old state and the switch appeared to revert.
//
// Delegated from the document so it keeps working for rows htmx swaps in.
document.addEventListener("change", function (event) {
  var toggle = event.target;
  if (!toggle || toggle.type !== "checkbox" || !toggle.closest(".toggle-form")) return;
  if (toggle.form && typeof toggle.form.requestSubmit === "function") toggle.form.requestSubmit();
});
