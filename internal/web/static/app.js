(function () {
  "use strict";

  // --- confirm dialogs ------------------------------------------------------
  document.querySelectorAll("form[data-confirm]").forEach(function (form) {
    form.addEventListener("submit", function (e) {
      if (!window.confirm(form.getAttribute("data-confirm"))) e.preventDefault();
    });
  });

  // --- "use global default" toggles ----------------------------------------
  document.querySelectorAll("input[data-inherit]").forEach(function (box) {
    var key = box.getAttribute("data-inherit");
    var target = document.querySelector('[data-inherit-target="' + key + '"]');
    if (!target) return;
    var apply = function () {
      target.classList.toggle("inherited", box.checked);
      target.querySelectorAll("input, select").forEach(function (el) { el.disabled = box.checked; });
    };
    box.addEventListener("change", apply);
    apply();
  });

  // --- schedule mode visibility -------------------------------------------
  document.querySelectorAll("select[data-schedule-mode]").forEach(function (sel) {
    var group = sel.closest("fieldset");
    var apply = function () {
      group.querySelectorAll("[data-mode]").forEach(function (el) {
        var modes = el.getAttribute("data-mode").split(/\s+/);
        el.classList.toggle("visible", modes.indexOf(sel.value) !== -1);
      });
    };
    sel.addEventListener("change", apply);
    apply();
  });

  // --- helpers ---------------------------------------------------------------
  function postJSON(url, body) {
    return fetch(url, {
      method: "POST",
      headers: { "Content-Type": "application/json", "Accept": "application/json" },
      credentials: "same-origin",
      body: JSON.stringify(body)
    }).then(function (r) { return r.json(); });
  }
  function debounce(fn, ms) {
    var t;
    return function () { clearTimeout(t); t = setTimeout(fn, ms); };
  }
  function field(form, name) {
    var el = form.querySelector('[name="' + name + '"]');
    return el ? (el.type === "checkbox" ? el.checked : el.value) : "";
  }

  // --- filename preview ------------------------------------------------------
  document.querySelectorAll("[data-naming]").forEach(function (box) {
    var form = box.closest("form");
    var out = box.querySelector("[data-naming-preview]");
    var preview = out.parentElement;
    var baseField = box.querySelector("[data-base-field]");
    var useDB = box.querySelector('[name="naming_use_db_name"]');
    var toggleBase = function () { if (baseField) baseField.style.display = useDB.checked ? "none" : ""; };
    useDB.addEventListener("change", toggleBase);
    toggleBase();

    var update = debounce(function () {
      var body = {
        database: field(form, "database") || "database",
        naming: {
          use_db_name: field(box, "naming_use_db_name"),
          base: field(box, "naming_base"),
          prefix: field(box, "naming_prefix"),
          suffix: field(box, "naming_suffix"),
          timestamp_prefix: field(box, "naming_ts_prefix"),
          timestamp_suffix: field(box, "naming_ts_suffix"),
          timestamp_format: field(box, "naming_ts_format"),
          gzip: field(box, "naming_gzip"),
          overwrite: field(box, "naming_overwrite")
        }
      };
      postJSON("/api/naming-preview", body).then(function (res) {
        preview.classList.toggle("err", !res.ok);
        out.textContent = res.ok ? res.filename : res.error;
      }).catch(function () { out.textContent = "preview unavailable"; });
    }, 250);

    box.querySelectorAll("input").forEach(function (el) { el.addEventListener("input", update); el.addEventListener("change", update); });
    var dbInput = form.querySelector('[name="database"]');
    if (dbInput) dbInput.addEventListener("input", update);
    update();
  });

  // --- test connection -------------------------------------------------------
  document.querySelectorAll("[data-connection-form]").forEach(function (form) {
    var btn = form.querySelector("[data-test-connection]");
    var out = form.querySelector("[data-test-result]");
    if (!btn) return;
    btn.addEventListener("click", function () {
      btn.disabled = true;
      out.className = "test-result";
      out.textContent = "Testing…";
      var body = {
        id: parseInt(form.getAttribute("data-connection-id") || "0", 10) || 0,
        host: field(form, "host"),
        port: parseInt(field(form, "port"), 10) || 3306,
        database: field(form, "database"),
        username: field(form, "username"),
        password: field(form, "password"),
        tls: field(form, "tls")
      };
      postJSON("/api/test-connection", body).then(function (res) {
        btn.disabled = false;
        if (res.ok) {
          out.className = "test-result ok";
          out.textContent = "Connected to MariaDB " + res.version + " in " + res.latency +
            (res.database_visible ? ". Database is visible." : ". Warning: the database is not visible to this user.");
          if (!res.database_visible) out.className = "test-result err";
        } else {
          out.className = "test-result err";
          out.textContent = "Failed: " + res.error;
        }
      }).catch(function () {
        btn.disabled = false;
        out.className = "test-result err";
        out.textContent = "Request failed.";
      });
    });
  });
})();
