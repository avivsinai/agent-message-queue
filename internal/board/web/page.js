(function () {
  "use strict";
  var root = document.documentElement;
  var live = root.hasAttribute("data-live");
  var announce = document.getElementById("announce");
  var views = Array.prototype.slice.call(document.querySelectorAll(".view"));
  var links = Array.prototype.slice.call(document.querySelectorAll("[data-view-link]"));
  var snapshot = {};
  try { snapshot = JSON.parse(document.getElementById("model").textContent); } catch (e) { /* the server still renders every record */ }
  var storageKey = "amq-board-ui:" + (snapshot.dir || "");
  var saved = {};
  try { saved = JSON.parse(window.sessionStorage.getItem(storageKey) || "{}"); } catch (e) { /* storage is optional */ }
  var current = "next";
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
    var rows = Array.prototype.slice.call(view.querySelectorAll(".strip"));
    var matched = 0;
    rows.forEach(function (row) {
      var text = [".title", ".c-ref", ".c-owner"].map(function (selector) {
        var node = row.querySelector(selector); return node ? node.textContent : "";
      }).join(" ").toLowerCase();
      var match = !query || text.indexOf(query) !== -1;
      matched += match ? 1 : 0;
      row.hidden = !match || view.dataset.view === "next" && !query && !allNext && matched > 5;
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
  function selectView(focus) {
    var requested = window.location.hash.replace(/^#view-/, "");
    var chosen = views.find(function (view) { return view.dataset.view === requested; });
    if (!chosen) { chosen = views.find(function (view) { return view.dataset.view === (saved.view || "next"); }) || views[0]; }
    if (!chosen) { return; }
    current = chosen.dataset.view;
    views.forEach(function (view) { view.hidden = view !== chosen; });
    links.forEach(function (link) {
      if (link.dataset.viewLink === current) { link.setAttribute("aria-current", "page"); }
      else { link.removeAttribute("aria-current"); }
    });
    filter(chosen);
    if (focus) {
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
  window.addEventListener("hashchange", function () { selectView(true); });
  document.addEventListener("toggle", function (event) { if (event.target.matches(".item-details")) { save(); } }, true);
  if (saved.focus) {
    var previous = document.getElementById(saved.focus);
    if (previous && previous.getClientRects().length) { previous.focus({preventScroll:true}); }
  } else if (saved.focusView === current) {
    var search = document.querySelector(".view:not([hidden]) .view-search");
    if (search) { search.focus({preventScroll:true}); }
  }

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
  var mobile = window.matchMedia("(max-width: 959px)");
  var secondary = document.querySelector(".secondary-nav");
  function placeSecondary() { if (secondary) { secondary.open = !mobile.matches; } }
  placeSecondary();
  mobile.addEventListener("change", placeSecondary);

  function manualCopy(button, command) {
    var row = button.closest(".strip");
    if (row) { row.querySelector(".item-details").open = true; }
    var scope = row || button.closest(".cmd");
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
  function tick() {
    if (!ageNode || isNaN(generated)) { return; }
    var minutes = Math.max(0, Math.floor((Date.now() - generated) / 60000));
    ageNode.textContent = minutes < 1 ? "(just now)" : "(" + minutes + " min ago)";
    ageNode.classList.toggle("is-stale", minutes > 5);
  }
  tick(); window.setInterval(tick, 30000);
  if (!live) { return; }
  var state = document.getElementById("live-state");
  var refresh = document.getElementById("refresh");
  var update = document.getElementById("apply-update");
  var pending = false;
  var checking = false;
  function check() {
    if (checking) { return Promise.resolve(); }
    checking = true;
    return fetch("model.json", {cache:"no-store"}).then(function (response) {
      if (!response.ok) { throw new Error("read failed"); }
      return response.json();
    }).then(function (model) {
      if (typeof model.generated_at !== "string") { throw new Error("invalid snapshot"); }
      var publicationError = (model.sources || []).find(function (source) { return source.name === "board-state" && source.status === "degraded"; });
      if (publicationError) {
        pending = false; update.hidden = true;
        state.classList.add("is-error"); state.textContent = publicationError.detail;
        return;
      }
      if (state.classList.contains("is-error")) { state.classList.remove("is-error"); state.textContent = "Published snapshot is available."; }
      if (model.generation !== snapshot.generation || model.generated_at !== snapshot.generated_at) {
        pending = true; update.hidden = false;
        state.classList.remove("is-error"); state.textContent = "New snapshot available. Update when you are ready.";
      }
    }).catch(function () {
      pending = false; update.hidden = true;
      state.classList.add("is-error"); state.textContent = "Refresh failed. Showing the existing snapshot.";
    }).finally(function () { checking = false; });
  }
  function applyUpdate() { save(); window.location.reload(); }
  update.addEventListener("click", applyUpdate);
  refresh.addEventListener("click", function () {
    if (pending) { applyUpdate(); return; }
    refresh.disabled = true; state.classList.remove("is-error"); state.textContent = "Checking for updates…";
    fetch("refresh", {cache:"no-store"}).then(function (response) {
      if (!response.ok) { throw new Error("refresh failed"); }
      return response.json();
    }).then(function () {
      state.textContent = "Refresh requested. The current snapshot remains visible.";
      return check();
    }).catch(function () {
      state.classList.add("is-error"); state.textContent = "Refresh failed. Showing the existing snapshot.";
    }).finally(function () { refresh.disabled = false; });
  });
  window.setInterval(check, 15000);
})();
