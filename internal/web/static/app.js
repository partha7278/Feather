// Progressive enhancement only. Every action on this panel is an ordinary form
// post and works with this file blocked; what follows adds confirmations, a
// live file name preview, and pickers for database and collection names.

(function () {
  "use strict";

  document.documentElement.classList.add("js");

  // --- the sidebar folds into a menu on narrow screens ----------------------
  //
  // Without this script the links simply stay visible.

  var sidebar = document.querySelector(".sidebar");
  var navToggle = document.querySelector(".nav-toggle");
  if (sidebar && navToggle) {
    navToggle.hidden = false;
    navToggle.addEventListener("click", function () {
      var open = sidebar.classList.toggle("open");
      navToggle.setAttribute("aria-expanded", open ? "true" : "false");
    });
    var closeNav = function (returnFocus) {
      if (!sidebar.classList.contains("open")) return;
      sidebar.classList.remove("open");
      navToggle.setAttribute("aria-expanded", "false");
      if (returnFocus) navToggle.focus();
    };
    document.addEventListener("keydown", function (event) {
      if (event.key === "Escape") closeNav(true);
    });
    // A tap outside the menu, or tabbing out of it, closes it.
    document.addEventListener("click", function (event) {
      if (!sidebar.contains(event.target)) closeNav(false);
    });
    sidebar.addEventListener("focusout", function (event) {
      if (event.relatedTarget && !sidebar.contains(event.relatedTarget)) closeNav(false);
    });
  }

  // --- theme switch ---------------------------------------------------------
  //
  // Without this script each button posts to /theme and the page reloads in
  // the new theme. With it, the cookie is written here and the page switches
  // in place. The server reads the same cookie to set data-theme on <html>.

  var themeSwitch = document.querySelector(".theme-switch");
  if (themeSwitch) {
    themeSwitch.addEventListener("click", function (event) {
      var button = event.target.closest && event.target.closest("button[name=theme]");
      if (!button) return;
      event.preventDefault();
      var theme = button.value;
      var secure = location.protocol === "https:" ? "; Secure" : "";
      if (theme === "light" || theme === "dark") {
        document.documentElement.setAttribute("data-theme", theme);
        document.cookie = "feather_theme=" + theme + "; Path=/; Max-Age=34560000; SameSite=Lax" + secure;
      } else {
        document.documentElement.removeAttribute("data-theme");
        document.cookie = "feather_theme=; Path=/; Max-Age=0; SameSite=Lax" + secure;
      }
      themeSwitch.querySelectorAll("button[name=theme]").forEach(function (other) {
        other.setAttribute("aria-pressed", other === button ? "true" : "false");
      });
    });
  }

  // --- row overflow menus (<details class="row-menu">) ----------------------
  //
  // Without this script a menu still opens and closes on its own summary.
  // With it, only one is open at a time, and a click outside or Escape
  // closes it.

  var menus = document.querySelectorAll("details.row-menu");
  if (menus.length) {
    // Tables scroll inside their card, which would clip the menu; it is
    // pinned to the viewport under its button instead, and follows it.
    var place = function (menu) {
      var list = menu.querySelector(".menu");
      var summary = menu.querySelector("summary");
      if (!list || !summary) return;
      var box = summary.getBoundingClientRect();
      list.classList.add("floating");
      list.style.top = Math.round(box.bottom + 4) + "px";
      list.style.right = Math.round(document.documentElement.clientWidth - box.right) + "px";
    };
    var closeMenus = function (except) {
      menus.forEach(function (menu) {
        if (menu !== except) menu.open = false;
      });
    };
    menus.forEach(function (menu) {
      menu.addEventListener("toggle", function () {
        if (!menu.open) return;
        closeMenus(menu);
        place(menu);
      });
    });
    document.addEventListener("click", function (event) {
      var inside = event.target.closest && event.target.closest("details.row-menu");
      closeMenus(inside);
    });
    var placeOpen = function () {
      document.querySelectorAll("details.row-menu[open]").forEach(place);
    };
    window.addEventListener("scroll", placeOpen, true);
    window.addEventListener("resize", placeOpen);
    document.addEventListener("keydown", function (event) {
      if (event.key !== "Escape") return;
      var open = document.querySelector("details.row-menu[open]");
      if (open) {
        open.open = false;
        var summary = open.querySelector("summary");
        if (summary) summary.focus();
      }
    });
  }

  // --- selects that submit their form on change -----------------------------
  //
  // An inline onchange handler is blocked by the CSP; this is its replacement.
  // The form keeps a submit button for when this file does not load.

  document.querySelectorAll("[data-autosubmit]").forEach(function (select) {
    select.addEventListener("change", function () {
      if (select.form) select.form.submit();
    });
  });
  document.querySelectorAll("[data-autosubmit-fallback]").forEach(function (el) {
    el.hidden = true;
  });

  // --- confirmations for anything destructive ------------------------------

  document.addEventListener("submit", function (event) {
    // On the form, or on the button that submitted it when only one of a
    // form's buttons is destructive.
    var message = event.target.getAttribute("data-confirm") ||
      (event.submitter && event.submitter.getAttribute("data-confirm"));
    if (message && !window.confirm(message)) {
      event.preventDefault();
    }
  });

  // --- forms that open in a popup ------------------------------------------
  //
  // Without this script the dialogs render inline as ordinary forms and the
  // buttons that open them stay hidden.

  if (typeof HTMLDialogElement === "function") {
    document.querySelectorAll("[data-dialog-buttons], [data-dialog-close]").forEach(function (el) {
      el.hidden = false;
    });
    document.querySelectorAll("[data-dialog-open]").forEach(function (button) {
      var dialog = document.getElementById(button.getAttribute("data-dialog-open"));
      if (!dialog) return;
      button.addEventListener("click", function () {
        dialog.showModal();
      });
    });
    document.querySelectorAll("dialog.modal").forEach(function (dialog) {
      // A click on the backdrop lands on the dialog itself, outside its box.
      dialog.addEventListener("click", function (event) {
        if (event.target === dialog) dialog.close();
      });
      dialog.querySelectorAll("[data-dialog-close]").forEach(function (button) {
        button.addEventListener("click", function () {
          dialog.close();
        });
      });
    });
    // A download leaves the page where it is, so the popup would stay open over
    // it. Closed after the form data has been gathered; the passphrases go too.
    document.querySelectorAll("form[data-dialog-download]").forEach(function (form) {
      form.addEventListener("submit", function () {
        window.setTimeout(function () {
          var dialog = form.closest("dialog");
          if (dialog) dialog.close();
          form.querySelectorAll('input[type="password"]').forEach(function (input) {
            input.value = "";
          });
        }, 0);
      });
    });
  }

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

    // Turning deletion on in the form asks first, as the list's switch does.
    var deletionWasOn = deletion.checked;
    if (deletion.form) {
      deletion.form.addEventListener("submit", function (event) {
        if (deletion.checked && !deletionWasOn && !window.confirm(
          "Turn on deletion for this config? Documents matching its filter will be removed from MongoDB " +
          "after every copy is stored and verified. Feather recommends saving it paused, then Count matches " +
          "and Dry run, before enabling the schedule.")) {
          event.preventDefault();
        }
      });

      // Count matches, Dry run and Run now act on the saved config: once the
      // form has unsaved edits they wait for Save.
      var savedActions = document.querySelectorAll("[data-saved-action]");
      var markDirty = function () {
        savedActions.forEach(function (button) {
          button.disabled = true;
          button.title = "Save changes first";
        });
      };
      deletion.form.addEventListener("input", markDirty);
      deletion.form.addEventListener("change", markDirty);
    }
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

  // --- import mode: the password is only for replacing -----------------------
  //
  // Without this script the password field is always shown, and the server
  // only reads it for a replace.

  var importModes = document.querySelectorAll("[data-import-mode]");
  if (importModes.length) {
    var applyImportMode = function () {
      var replace = false;
      importModes.forEach(function (radio) {
        if (radio.checked && radio.value === "replace") replace = true;
      });
      document.querySelectorAll("[data-replace-only]").forEach(function (el) {
        el.hidden = !replace;
      });
      document.querySelectorAll("[data-replace-confirm]").forEach(function (button) {
        if (replace) {
          button.setAttribute("data-confirm", button.getAttribute("data-replace-confirm"));
        } else {
          button.removeAttribute("data-confirm");
        }
      });
    };
    importModes.forEach(function (radio) {
      radio.addEventListener("change", applyImportMode);
    });
    applyImportMode();
  }

  // --- database and collection pickers --------------------------------------

  var source = document.querySelector("[data-source]");
  var database = document.querySelector("[data-database]");
  var collection = document.querySelector("[data-collection]");

  // The account picker's value is "engine:alias". Choosing an account of the
  // other engine relabels the namespace fields, swaps the filter's language
  // and hides what only MongoDB has. The server renders the saved state, so
  // this only matters for a change made on the page.
  var applyEngine = function () {
    if (!source) return;
    var engine = source.value.indexOf("postgres:") === 0 ? "postgres" : "mongodb";
    document.querySelectorAll("[data-engine-only]").forEach(function (el) {
      el.hidden = el.getAttribute("data-engine-only") !== engine;
    });
    document.querySelectorAll("[data-label-" + engine + "]").forEach(function (el) {
      el.textContent = el.getAttribute("data-label-" + engine);
    });
    var filter = document.querySelector("[data-filter]");
    if (filter) filter.placeholder = filter.getAttribute("data-placeholder-" + engine) || "";
    if (database) {
      database.required = engine !== "postgres";
      database.placeholder = engine === "postgres" ? "public" : "";
    }
  };
  if (source) {
    source.addEventListener("change", applyEngine);
    applyEngine();
  }

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
