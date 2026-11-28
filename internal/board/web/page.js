(function () {
  "use strict";
  var root = document.documentElement;
  var live = root.hasAttribute("data-live");
  var announce = document.getElementById("announce");
  var views = Array.prototype.slice.call(document.querySelectorAll(".view"));
  var links = Array.prototype.slice.call(document.querySelectorAll("[data-view-link]"));
  var secondary = document.querySelector(".secondary-nav");
  var snapshot = {};
  try { snapshot = JSON.parse(document.getElementById("model").textContent); } catch (e) { /* the server still renders every record */ }
  var storageKey = "amq-board-ui:" + (snapshot.dir || "");
  var saved = {};
  try { saved = JSON.parse(window.sessionStorage.getItem(storageKey) || "{}"); } catch (e) { /* storage is optional */ }
  var current = "flow";
  var allNext = saved.allNext === true;

  function say(text) { if (announce) { announce.textContent = text; } }
  function save() {
    var queries = {};
    views.forEach(function (view) {
      var input = view.querySelector(".view-search");
      if (input) { queries[view.dataset.view] = input.value; }
    });
    var focused = document.activeElement;
    var open = Array.prototype.slice.call(document.querySelectorAll(".item-details[open]")).map(function (node) { return node.id; });
    var focusView = focused && focused.matches(".view-search") ? focused.closest(".view").dataset.view : "";
    try { window.sessionStorage.setItem(storageKey, JSON.stringify({view:current, queries:queries, open:open, allNext:allNext, focus:focused ? focused.id : "", focusView:focusView})); } catch (e) { /* navigation and copy work without storage */ }
  }
  function filter(view) {
    var input = view.querySelector(".view-search");
    if (!input) { return; }
    var query = input.value.trim().toLowerCase();
    var rows = Array.prototype.slice.call(view.querySelectorAll(".strip, .flow-card, .station"));
    var matched = 0;
    rows.forEach(function (row) {
      var text = [".title", ".c-ref", ".c-owner"].map(function (selector) {
        var node = row.querySelector(selector); return node ? node.textContent : "";
      }).join(" ").toLowerCase();
      var match = !query || text.indexOf(query) !== -1;
      matched += match ? 1 : 0;
      row.hidden = !match || view.dataset.view === "next" && !query && !allNext && matched > 5;
    });
    Array.prototype.slice.call(view.querySelectorAll(".request-group")).forEach(function (group) {
      group.hidden = !!query && !group.querySelector(".strip:not([hidden])");
    });
    var note = view.querySelector(".filter-result");
    if (note) { note.hidden = !query || matched !== 0; }
    var list = view.querySelector(".work-list");
    if (list) { list.hidden = !!query && matched === 0; }
    var showAll = view.querySelector(".show-all");
    if (showAll) {
      showAll.hidden = !!query || allNext || rows.length <= 5;
      showAll.textContent = "Show all " + rows.length + " known next actions";
    }
  }
  // A fragment names a view (#view-inbox) or a record inside one, such as
  // an Inbox item a Needs you tile links to; a record opens its view.
  function hashTarget() {
    var id = "";
    try { id = decodeURIComponent(window.location.hash.slice(1)); } catch (e) { return null; }
    var node = id ? document.getElementById(id) : null;
    return node && !node.matches(".view") && node.closest(".view") ? node : null;
  }
  function selectView(focus) {
    var requested = window.location.hash.replace(/^#view-/, "");
    var target = hashTarget();
    var chosen = target ? target.closest(".view") : views.find(function (view) { return view.dataset.view === requested; });
    if (!chosen) { chosen = views.find(function (view) { return view.dataset.view === (saved.view || "flow"); }) || views[0]; }
    if (!chosen) { return; }
    current = chosen.dataset.view;
    root.setAttribute("data-view", current);
    views.forEach(function (view) { view.hidden = view !== chosen; });
    links.forEach(function (link) {
      if (link.dataset.viewLink === current) { link.setAttribute("aria-current", "page"); }
      else { link.removeAttribute("aria-current"); }
    });
    filter(chosen);
    placeSecondary();
    // One view shows at a time, so a view link opens it from the top with
    // the masthead and navigation in place instead of jumping to its
    // anchor. The scroll lands before the browser's own anchor jump for
    // this fragment, which would otherwise move the page and take the
    // bar off screen.
    window.scrollTo(0, 0);
    if (target) {
      var title = target.querySelector("h2") || target;
      target.scrollIntoView({block:"start"});
      title.tabIndex = -1; title.focus({preventScroll:true});
    } else if (focus) {
      var heading = chosen.querySelector("h1");
      if (heading) { heading.tabIndex = -1; heading.focus({preventScroll:true}); }
    }
    save();
  }
  views.forEach(function (view) {
    var input = view.querySelector(".view-search");
    if (input) {
      input.value = saved.queries && saved.queries[view.dataset.view] || "";
      input.addEventListener("input", function () { filter(view); save(); });
    }
    var clear = view.querySelector(".clear-search");
    if (clear) { clear.addEventListener("click", function () { input.value = ""; filter(view); input.focus(); save(); }); }
    var more = view.querySelector(".show-all");
    if (more) { more.addEventListener("click", function () { allNext = true; filter(view); save(); }); }
  });
  (saved.open || []).forEach(function (id) { var detail = document.getElementById(id); if (detail && detail.matches("details")) { detail.open = true; } });
  root.classList.add("enhanced");
  selectView(false);
  // A click on a view link opens the view itself and keeps the page at the
  // top. pushState records the address without a fragment navigation: a
  // young document keeps retrying the jump for a fragment set through
  // location.hash, which lands after this scroll and moves the bar off
  // screen.
  document.addEventListener("click", function (event) {
    var link = event.target.closest("[data-view-link]");
    if (!link) { return; }
    event.preventDefault();
    var id = link.getAttribute("href").slice(1);
    if (window.location.hash !== "#" + id) { history.pushState(null, "", "#" + id); }
    selectView(true);
  });
  window.addEventListener("popstate", function () { selectView(true); });
  window.addEventListener("hashchange", function () { selectView(true); });
  // The browser scrolls to the fragment after load, so the view opens from
  // the top once that scroll has happened.
  window.addEventListener("load", function () { window.setTimeout(function () { if (window.location.hash === "#view-" + current) { window.scrollTo(0, 0); document.body.setAttribute("tabindex", "-1"); document.body.focus({preventScroll: true}); document.body.removeAttribute("tabindex"); } }, 0); });
  document.addEventListener("toggle", function (event) { if (event.target.matches(".item-details")) { save(); } }, true);
  if (saved.focus) {
    var previous = document.getElementById(saved.focus);
    if (previous && previous.getClientRects().length) { previous.focus({preventScroll:true}); }
  } else if (saved.focusView === current) {
    var search = document.querySelector(".view:not([hidden]) .view-search");
    if (search) { search.focus({preventScroll:true}); }
  }

  // More is one closed menu under the bar on every view and width.
  function placeSecondary() { if (secondary) { secondary.open = false; } }
  function closeMenu(focus) {
    if (!secondary || !secondary.open) { return false; }
    secondary.open = false;
    if (focus) { secondary.querySelector("summary").focus({preventScroll:true}); }
    return true;
  }
  document.addEventListener("click", function (event) { if (secondary && !secondary.contains(event.target)) { closeMenu(false); } });

  function sheets() { return Array.prototype.slice.call(document.querySelectorAll(".card-sheet[open]")); }
  function closeSheet(sheet, focus) {
    sheet.open = false;
    if (focus) { sheet.querySelector("summary").focus({preventScroll:true}); }
  }
  document.addEventListener("toggle", function (event) {
    var sheet = event.target;
    if (!sheet.matches || !sheet.matches(".card-sheet") || !sheet.open) { return; }
    sheets().forEach(function (other) { if (other !== sheet) { closeSheet(other, false); } });
    var close = sheet.querySelector(".sheet-close");
    if (close) { close.focus({preventScroll:true}); }
  }, true);
  document.addEventListener("keydown", function (event) {
    if (event.key !== "Escape") { return; }
    if (closeMenu(!!secondary && secondary.contains(document.activeElement))) { return; }
    sheets().forEach(function (sheet) { closeSheet(sheet, true); });
  });
  document.addEventListener("click", function (event) {
    var close = event.target.closest(".sheet-close");
    if (close) { closeSheet(close.closest(".card-sheet"), true); }
  });

  var appearance = document.getElementById("appearance");
  var theme = "system";
  try { theme = window.localStorage.getItem("amq-board-appearance") || theme; } catch (e) { /* use the system theme */ }
  function applyTheme(value) {
    if (value === "light" || value === "dark") { root.setAttribute("data-theme", value); }
    else { value = "system"; root.removeAttribute("data-theme"); }
    appearance.value = value;
  }
  applyTheme(theme);
  appearance.addEventListener("change", function () {
    applyTheme(appearance.value);
    try { window.localStorage.setItem("amq-board-appearance", appearance.value); } catch (e) { /* theme still applies to this view */ }
  });

  function manualCopy(button, command) {
    var row = button.closest(".strip");
    var details = row && row.querySelector(".item-details");
    if (details) { details.open = true; }
    var scope = row || button.closest(".cmd") || button.closest(".sheet");
    var field = scope && Array.prototype.slice.call(scope.querySelectorAll("textarea")).find(function (node) { return node.value === command; });
    button.classList.add("is-failed");
    if (field) { field.focus(); field.select(); say("Copy unavailable. The command is selected for manual copying."); }
    else { say("Copy unavailable. Open Details and select the command manually."); }
  }
  document.addEventListener("click", function (event) {
    var button = event.target.closest("button.copy");
    if (!button) { return; }
    var command = button.getAttribute("data-command");
    if (command === null) { return; }
    var label = button.textContent;
    if (!navigator.clipboard || !window.isSecureContext) { manualCopy(button, command); return; }
    navigator.clipboard.writeText(command).then(function () {
      button.classList.remove("is-failed"); button.textContent = "Copied";
      say("Command copied. Paste it in your terminal.");
      window.setTimeout(function () { button.textContent = label; }, 1500);
    }, function () { manualCopy(button, command); });
  });

  var attachableTasks = new Map();
  function addTask(task) { if (task && task.id) { attachableTasks.set(task.id, task); } }
  (snapshot.catalogue || []).forEach(addTask);
  (snapshot.columns || []).forEach(function (column) { (column.cards || []).forEach(function (card) { if (!attachableTasks.has(card.id)) { addTask(card); } }); });
  (snapshot.historical || []).forEach(function (card) { if (!attachableTasks.has(card.id)) { addTask(card); } });
  var taskLabels = new Map();
  document.querySelectorAll("#board-tasks option").forEach(function (option) {
    if (attachableTasks.has(option.dataset.taskId)) { taskLabels.set(option.value, option.dataset.taskId); }
  });
  function quoteArg(value) { return "'" + value.replace(/'/g, "'\\''") + "'"; }
  document.addEventListener("input", function (event) {
    if (!event.target.matches(".attach-task")) { return; }
    var control = event.target.closest(".attach-control");
    var button = control.querySelector("button.copy"), field = control.querySelector("textarea");
    var value = event.target.value, taskID = taskLabels.get(value) || value.trim(), key = control.dataset.message;
    taskID = (snapshot.redirects || {})[taskID] || taskID;
    var task = attachableTasks.get(taskID), selection = control.querySelector(".attach-selection");
    if (!task || /[\u0000-\u001f\u007f]/.test(taskID + key)) {
      button.disabled = true; button.removeAttribute("data-command"); field.value = "";
      selection.textContent = "Select a task from the list, or paste a task ID.";
      return;
    }
    var note = "Attach evidence to " + task.title + ".";
    if (task.cancelled) { note += " This task is cancelled; attaching evidence will not reopen it."; }
    else if (task.historical || task.recorded_state === "done") { note += " Its historical state will stay the same."; }
    selection.textContent = note;
    var command = "amq-board attach --task " + quoteArg(taskID) + " --message " + quoteArg(key);
    field.value = command; button.setAttribute("data-command", command); button.disabled = false;
  });

  var timeNode = document.querySelector(".mast-gen time");
  var ageNode = document.getElementById("page-age");
  var generated = timeNode ? Date.parse(timeNode.dateTime) : NaN;
  var staleNode = document.querySelector(".snapshot-stale");
  var freshnessNode = document.getElementById("freshness-window");
  // No bound from the server means no client timer: the page must not guess one.
  var freshnessWindow = freshnessNode ? Number(freshnessNode.textContent) : Infinity;
  function ageText(milliseconds) {
    var seconds = Math.floor(milliseconds / 1000);
    if (seconds < 60) { return seconds + "s"; }
    var minutes = Math.floor(seconds / 60);
    if (minutes < 60) { return minutes + "m"; }
    var hours = Math.floor(minutes / 60);
    if (hours < 48) { return hours + "h"; }
    return Math.floor(hours / 24) + "d";
  }
  function tick() {
    if (!ageNode || isNaN(generated)) { return; }
    var seconds = Math.max(0, Date.now() - generated) / 1000;
    var minutes = Math.floor(seconds / 60);
    var old = seconds > freshnessWindow;
    // The timer alone moves the banner of the shown snapshot: the header
    // time and the banner describe the cards the page SHOWS, never a pane
    // that is only available.
    ageNode.textContent = minutes < 1 ? "(just now)" : "(" + minutes + " min ago)";
    ageNode.classList.toggle("is-stale", old);
    if (!staleNode) { return; }
    // The one timer advances or clears the one banner of the shown
    // snapshot; it does not render or dismiss a pending pane.
    staleNode.hidden = !old;
    staleNode.textContent = old ? "STALE: published " + ageText(seconds * 1000) + " ago (" + timeNode.dateTime + "). Every number below is from then. Run amq-board sync." : "";
  }
  tick(); window.setInterval(tick, 30000);
  if (!live) { return; }
  function applyFreshPane(model) {
    // pane.json is a bounded freshness face, not the whole board model.
    // A newer pane that is only available changes the Update affordance
    // and the command data, and nothing else: the header time and the
    // banner still describe the shown cards until Update replaces them.
    if (model.dir) { snapshot.dir = model.dir; }
    if (model.generation) { snapshot.generation = model.generation; }
  }
  var state = document.getElementById("live-state");
  var refresh = document.getElementById("refresh");
  var pending = false;
  var checking = false;
  // One button refreshes, and applies a newer snapshot once one waits.
  function setPending(value) {
    pending = value;
    refresh.textContent = value ? "Update" : "Refresh";
  }
  function check() {
    if (checking) { return Promise.resolve(); }
    checking = true;
    return fetch("pane.json", {cache:"no-store"}).then(function (response) {
      if (!response.ok) { throw new Error("read failed"); }
      return response.json();
    }).then(function (model) {
      if (typeof model.generated_at !== "string") { throw new Error("invalid snapshot"); }
      var publicationError = (model.sources || []).find(function (source) { return source.name === "board-state" && source.status === "degraded"; });
      if (publicationError) {
        setPending(false);
        state.classList.add("is-error"); state.textContent = publicationError.detail;
        return;
      }
      if (typeof model.stale !== "boolean" || !Number.isFinite(model.published_age_seconds)) { throw new Error("invalid freshness"); }
      var previousPublication = snapshot.published_at || snapshot.generated_at;
      var newer = model.generation !== snapshot.generation || (model.published_at || model.generated_at) !== previousPublication;
      state.classList.remove("is-error");
      if (newer) {
        setPending(true);
        state.textContent = "New snapshot available.";
        applyFreshPane(model);
      } else if (!pending) {
        // Idle shows no status sentence.
        state.textContent = "";
      }
    }).catch(function () {
      // A failed poll keeps a waiting update: the button still applies it.
      state.classList.add("is-error"); state.textContent = "Refresh failed. Showing the existing snapshot.";
    }).finally(function () { checking = false; });
  }
  function applyUpdate() {
    save(); window.location.reload();
  }
  refresh.addEventListener("click", function () {
    if (pending) { applyUpdate(); return; }
    refresh.disabled = true; state.classList.remove("is-error"); state.textContent = "Checking for updates…";
    fetch("refresh", {cache:"no-store"}).then(function (response) {
      if (!response.ok) { throw new Error("refresh failed"); }
      return response.json();
    }).then(function () {
      state.textContent = "";
      return check();
    }).catch(function () {
      state.classList.add("is-error"); state.textContent = "Refresh failed. Showing the existing snapshot.";
    }).finally(function () { refresh.disabled = false; });
  });
  window.setInterval(check, 15000);
})();
