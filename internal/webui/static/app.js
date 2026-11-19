// Web UI 初始化脚本：把页面 meta 里的防跨站请求伪造令牌放进每个 htmx
// 请求的 X-CSRF-Token 请求头，页面模板本身保持无内联脚本。
(function () {
  "use strict";
  var meta = document.querySelector('meta[name="csrf-token"]');
  var token = meta === null ? "" : meta.getAttribute("content");

  function updateShareScopeWarnings(form) {
    var rows = Array.prototype.slice.call(form.querySelectorAll("[data-share-scope-device]"));
    rows.forEach(function (row) {
      var warning = row.querySelector("[data-share-scope-warning]");
      var checkbox = row.querySelector('input[type="checkbox"]');
      if (warning === null || checkbox === null) {
        return;
      }
      var device = row.getAttribute("data-share-scope-device");
      var hasUncheckedOtherEntry = device !== "" && rows.some(function (other) {
        var otherCheckbox = other.querySelector('input[type="checkbox"]');
        return other !== row && other.getAttribute("data-share-scope-device") === device &&
          otherCheckbox !== null && !otherCheckbox.checked;
      });
      warning.hidden = !checkbox.checked || !hasUncheckedOtherEntry;
    });
  }

  function updateAllShareScopeWarnings() {
    document.querySelectorAll("[data-share-scope-form]").forEach(updateShareScopeWarnings);
  }

  function updateRevealTarget(checkbox) {
    var target = document.querySelector("[data-share-reveal-target]");
    if (target !== null) {
      target.hidden = !checkbox.checked;
    }
  }

  // 备注必填表单：备注为空时保持创建按钮禁用；服务端存储层仍会再校验。
  function updateRequiredNoteForm(form) {
    var note = form.querySelector("[data-required-note]");
    var submit = form.querySelector("[data-note-submit]");
    if (note === null || submit === null) {
      return;
    }
    submit.disabled = note.value.trim() === "";
  }

  function initRequiredNoteForms() {
    document.querySelectorAll("[data-required-note-form]").forEach(function (form) {
      var note = form.querySelector("[data-required-note]");
      if (note === null) {
        return;
      }
      updateRequiredNoteForm(form);
      note.addEventListener("input", function () {
        updateRequiredNoteForm(form);
      });
    });
  }

  // 共用服务器勾选组件：搜索与三类筛选、三态全选与底部实时差异统计。
  // 隐藏行仍留在文档里，勾选值随表单提交，因此筛选不会丢选择。
  function initServerSelection(root) {
    var form = root.querySelector("form");
    if (form === null) {
      return;
    }
    var search = form.querySelector("[data-selection-search]");
    var site = form.querySelector("[data-selection-site]");
    var network = form.querySelector("[data-selection-network]");
    var profile = form.querySelector("[data-selection-profile]");
    var toggle = form.querySelector("[data-selection-toggle]");
    var summary = form.querySelector("[data-selection-summary]");
    var next = form.querySelector("[data-selection-next]");
    var visibleCount = form.querySelector("[data-selection-visible-count]");
    var rows = Array.prototype.slice.call(form.querySelectorAll("[data-selection-row]"));

    function attributeValues(row, name) {
      var value = row.getAttribute(name);
      return value === "" ? [] : value.split(",");
    }

    function rowVisible(row) {
      var keyword = search === null ? "" : search.value.trim().toLowerCase();
      if (keyword !== "" && row.getAttribute("data-search-text").toLowerCase().indexOf(keyword) < 0) {
        return false;
      }
      if (site !== null && site.value !== "" && attributeValues(row, "data-server-site").indexOf(site.value) < 0) {
        return false;
      }
      if (network !== null && network.value !== "" && attributeValues(row, "data-server-network").indexOf(network.value) < 0) {
        return false;
      }
      if (profile !== null && profile.value !== "" && attributeValues(row, "data-server-profile").indexOf(profile.value) < 0) {
        return false;
      }
      return true;
    }

    function checkbox(row) {
      return row.querySelector('input[type="checkbox"]');
    }

    function refresh() {
      var visible = 0;
      var checked = 0;
      var added = 0;
      var removed = 0;
      rows.forEach(function (row) {
        var show = rowVisible(row);
        row.hidden = !show;
        var box = checkbox(row);
        if (show) {
          visible++;
          if (box !== null && box.checked) {
            checked++;
          }
        }
        var current = row.getAttribute("data-current") === "1";
        if (box !== null && box.checked && !current) {
          added++;
        }
        if (box !== null && !box.checked && current) {
          removed++;
        }
      });
      if (toggle !== null) {
        var state = visible === 0 || checked === 0 ? "none" : checked === visible ? "all" : "some";
        toggle.setAttribute("data-selection-state", state);
        toggle.setAttribute("aria-checked", state === "all" ? "true" : state === "some" ? "mixed" : "false");
      }
      if (visibleCount !== null) {
        visibleCount.textContent = visible + " 台";
      }
      if (summary !== null) {
        summary.textContent = "新增 " + added + " 台、移除 " + removed + " 台";
      }
      if (next !== null) {
        next.disabled = added === 0 && removed === 0;
      }
    }

    if (search !== null) {
      search.addEventListener("input", refresh);
    }
    [site, network, profile].forEach(function (select) {
      if (select !== null) {
        select.addEventListener("change", refresh);
      }
    });
    if (toggle !== null) {
      toggle.addEventListener("click", function () {
        var hasUncheckedVisible = rows.some(function (row) {
          var box = checkbox(row);
          return !row.hidden && box !== null && !box.checked;
        });
        rows.forEach(function (row) {
          if (row.hidden) {
            return;
          }
          var box = checkbox(row);
          if (box !== null) {
            box.checked = hasUncheckedVisible;
          }
        });
        refresh();
      });
    }
    rows.forEach(function (row) {
      var box = checkbox(row);
      if (box !== null) {
        box.addEventListener("change", refresh);
      }
    });
    refresh();
  }

  document.body.addEventListener("htmx:configRequest", function (event) {
    event.detail.headers["X-CSRF-Token"] = token;
  });
  document.body.addEventListener("click", function (event) {
    var button = event.target.closest("[data-copy-target]");
    if (button === null) {
      return;
    }
    var source = document.getElementById(button.getAttribute("data-copy-target"));
    if (source === null) {
      return;
    }
    var text = source.textContent;
    function finished() {
      var oldText = button.textContent;
      button.textContent = "已复制";
      window.setTimeout(function () {
        button.textContent = oldText;
      }, 1500);
    }
    if (navigator.clipboard && navigator.clipboard.writeText) {
      navigator.clipboard.writeText(text).then(finished, function () {
        window.prompt("请手动复制：", text);
      });
      return;
    }
    window.prompt("请手动复制：", text);
  });
  document.body.addEventListener("change", function (event) {
    var autoSubmitForm = event.target.closest("[data-auto-submit]");
    if (autoSubmitForm !== null && event.target.matches("input[type=checkbox], input[type=radio]")) {
      autoSubmitForm.submit();
    }
    var form = event.target.closest("[data-share-scope-form]");
    if (form !== null) {
      updateShareScopeWarnings(form);
    }
    if (event.target.hasAttribute && event.target.hasAttribute("data-share-reveal-checkbox")) {
      updateRevealTarget(event.target);
    }
  });
  updateAllShareScopeWarnings();
  initRequiredNoteForms();
  document.querySelectorAll("[data-server-selection]").forEach(initServerSelection);
})();
