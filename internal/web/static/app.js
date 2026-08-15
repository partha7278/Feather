// Progressive enhancement only. Every action on this panel is an ordinary form
// post and works with this file blocked; what follows adds confirmations, a
// live file name preview, and pickers for database and collection names.

(function () {
  "use strict";

  // --- confirmations for anything destructive ------------------------------

  document.addEventListener("submit", function (event) {
    var message = event.target.getAttribute("data-confirm");
    if (message && !window.confirm(message)) {
      event.preventDefault();
    }
  });

  // --- file name template preview ------------------------------------------

  var template = document.querySelector("[data-key-template]");
  var preview = document.querySelector("[data-key-preview]");

  if (template && preview) {
    var pad = function (n, width) {
      var s = String(n);
      while (s.length < width) s = "0" + s;
      return s;
    };

    var render = function () {
      var now = new Date();
      var codec = document.getElementById("codec");
      var ext = codec && codec.value === "zstd" ? ".ndjson.zst" : ".ndjson.gz";
      var values = {
        "{config}": value("name") || "config",
        "{db}": value("database") || "database",
        "{collection}": value("collection") || "collection",
        "{runId}": "0f5b1c9a-3d4e-4a1b-9c8d-7e6f5a4b3c2d",
        "{seq}": "00001",
        "{yyyy}": String(now.getUTCFullYear()),
        "{MM}": pad(now.getUTCMonth() + 1, 2),
        "{dd}": pad(now.getUTCDate(), 2),
        "{HH}": pad(now.getUTCHours(), 2),
        "{mm}": pad(now.getUTCMinutes(), 2),
        "{ts}":
          String(now.getUTCFullYear()) + pad(now.getUTCMonth() + 1, 2) + pad(now.getUTCDate(), 2) +
          "T" + pad(now.getUTCHours(), 2) + pad(now.getUTCMinutes(), 2) + pad(now.getUTCSeconds(), 2) + "Z",
        "{ext}": ext
      };

      var out = template.value;
      Object.keys(values).forEach(function (token) {
        out = out.split(token).join(values[token]);
      });
      preview.textContent = out.replace(/^\//, "") || "—";
    };

    var value = function (id) {
      var el = document.getElementById(id);
      return el ? el.value.trim() : "";
    };

    ["keyTemplate", "name", "database", "collection", "codec"].forEach(function (id) {
      var el = document.getElementById(id);
      if (el) {
        el.addEventListener("input", render);
        el.addEventListener("change", render);
      }
    });
    render();
  }

  // --- schedule kind switch -------------------------------------------------

  var kind = document.querySelector("[data-schedule-kind]");
  if (kind) {
    var applyKind = function () {
      document.querySelectorAll("[data-schedule]").forEach(function (field) {
        field.hidden = field.getAttribute("data-schedule") !== kind.value;
      });
    };
    kind.addEventListener("change", applyKind);
    applyKind();
  }

  // --- deletion switch ------------------------------------------------------

  var deletion = document.querySelector("[data-deletion]");
  var deletionDetails = document.querySelector("[data-deletion-details]");
  if (deletion && deletionDetails) {
    deletion.addEventListener("change", function () {
      deletionDetails.hidden = !deletion.checked;
    });
  }

  // --- the provider decides which half of the form applies ------------------
  //
  // Both halves are in the page so the form still works with this script
  // disabled; the server reads only the fields its provider uses.

  var provider = document.querySelector("[data-provider]");
  var pathStyle = document.querySelector("[data-path-style]");
  if (provider) {
    var providerFields = document.querySelectorAll("[data-provider-fields]");

    var showProviderFields = function () {
      var family = provider.value === "gdrive" ? "gdrive" : "s3";
      providerFields.forEach(function (block) {
        block.hidden = block.getAttribute("data-provider-fields") !== family;
      });
    };

    provider.addEventListener("change", function () {
      // Oracle only speaks path-style addressing.
      if (pathStyle && provider.value === "oci") {
        pathStyle.checked = true;
      }
      showProviderFields();
    });
    showProviderFields();
  }

  // --- database and collection pickers --------------------------------------

  var source = document.querySelector("[data-source]");
  var database = document.querySelector("[data-database]");
  var collection = document.querySelector("[data-collection]");

  if (source && database && collection) {
    var fill = function (listId, names) {
      var list = document.getElementById(listId);
      if (!list) return;
      list.innerHTML = "";
      (names || []).forEach(function (name) {
        var option = document.createElement("option");
        option.value = name;
        list.appendChild(option);
      });
    };

    var loadDatabases = function () {
      if (!source.value) return;
      fetch("/api/collections?source=" + encodeURIComponent(source.value))
        .then(function (r) { return r.json(); })
        .then(function (body) { fill("database-options", body.databases); })
        .catch(function () { /* the field is free text; a failed lookup is not fatal */ });
    };

    var loadCollections = function () {
      if (!source.value || !database.value) return;
      fetch("/api/collections?source=" + encodeURIComponent(source.value) +
            "&database=" + encodeURIComponent(database.value))
        .then(function (r) { return r.json(); })
        .then(function (body) { fill("collection-options", body.collections); })
        .catch(function () {});
    };

    source.addEventListener("change", function () { loadDatabases(); loadCollections(); });
    database.addEventListener("change", loadCollections);
    database.addEventListener("focus", loadDatabases);
    collection.addEventListener("focus", loadCollections);
    if (source.value) { loadDatabases(); loadCollections(); }
  }
})();
