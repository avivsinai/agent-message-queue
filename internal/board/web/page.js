(function () {
  "use strict";

  var announce = document.getElementById("announce");

  // Copy: the command is the text of the code element beside the button.
  // When the clipboard refuses, the command is selected instead and the
  // button says so; it never claims a copy that did not happen.
  function selectText(node) {
    var range = document.createRange();
    range.selectNodeContents(node);
    var sel = window.getSelection();
    sel.removeAllRanges();
    sel.addRange(range);
  }

  function legacyCopy(node) {
    selectText(node);
    try {
      return document.execCommand("copy");
    } catch (e) {
      return false;
    }
  }

  function settle(button, code, ok) {
    button.textContent = ok ? "Copied" : "Selected";
    button.classList.toggle("is-copied", ok);
    button.classList.toggle("is-failed", !ok);
    announce.textContent = ok ? "Copied: " + code.textContent : "Copy failed. The command is selected; copy it with the keyboard.";
    if (!ok) {
      selectText(code);
    }
    window.clearTimeout(button.copyTimer);
    button.copyTimer = window.setTimeout(function () {
      button.textContent = "Copy";
      button.classList.remove("is-copied", "is-failed");
    }, 1500);
  }

  document.addEventListener("click", function (event) {
    var button = event.target.closest("button.copy");
    if (!button) {
      return;
    }
    var code = button.parentElement.querySelector("code");
    if (!code) {
      return;
    }
    var text = code.textContent;
    if (navigator.clipboard && window.isSecureContext) {
      navigator.clipboard.writeText(text).then(
        function () { settle(button, code, true); },
        function () { settle(button, code, legacyCopy(code)); }
      );
    } else {
      settle(button, code, legacyCopy(code));
    }
  });

  // Page age: the strips are a snapshot; the header says how old it is and
  // marks it stale after five minutes.
  var stamp = document.querySelector(".mast-gen time");
  var ageNode = document.getElementById("page-age");
  var generated = stamp ? Date.parse(stamp.dateTime) : NaN;

  function ago(ms) {
    var m = Math.floor(ms / 60000);
    if (m < 1) { return "less than a minute ago"; }
    if (m < 60) { return m + " min ago"; }
    var h = Math.floor(m / 60);
    if (h < 48) { return h + " h ago"; }
    return Math.floor(h / 24) + " d ago";
  }

  function tick() {
    if (isNaN(generated) || !ageNode) {
      return;
    }
    var ms = Math.max(0, Date.now() - generated);
    var stale = ms > 5 * 60000;
    ageNode.textContent = "(" + ago(ms) + (stale ? ", stale: regenerate" : "") + ")";
    ageNode.classList.toggle("is-stale", stale);
  }

  tick();
  window.setInterval(tick, 30000);

  // Filter: matches handle, ref, and title text; hidden strips are counted
  // per bay so nothing disappears silently.
  var input = document.getElementById("filter");
  var bays = Array.prototype.slice.call(document.querySelectorAll(".bay")).map(function (bay) {
    var strips = Array.prototype.slice.call(bay.querySelectorAll(".strip")).map(function (strip) {
      var parts = [".c-ref", ".title", ".c-owner"].map(function (sel) {
        var n = strip.querySelector(sel);
        return n ? n.textContent : "";
      });
      return { node: strip, text: parts.join(" ").toLowerCase() };
    });
    return { strips: strips, note: bay.querySelector(".filtered") };
  });

  function applyFilter() {
    var q = input.value.trim().toLowerCase();
    bays.forEach(function (bay) {
      var hidden = 0;
      bay.strips.forEach(function (s) {
        var hide = q !== "" && s.text.indexOf(q) < 0;
        s.node.hidden = hide;
        if (hide) { hidden++; }
      });
      if (bay.note) {
        bay.note.hidden = hidden === 0;
        bay.note.textContent = hidden === 0 ? "" : hidden + " hidden by filter";
      }
    });
  }

  if (input) {
    input.addEventListener("input", applyFilter);
  }
})();
