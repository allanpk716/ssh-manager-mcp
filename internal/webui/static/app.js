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

  // 服务器列表多选：勾选卡片后打开底部批量操作栏。资源链接把选中标识符
  // 带到 #301 的共用服务器勾选页，仍由既有确认流程执行写入。
  function initServerListBatch(root) {
    var cards = Array.prototype.slice.call(root.querySelectorAll("[data-server-card]"));
    var bar = document.querySelector("[data-server-selection-bar]");
    var drawer = document.querySelector("[data-server-batch-drawer]");
    var count = document.querySelector("[data-server-selected-count]");
    if (cards.length === 0 || bar === null || drawer === null) {
      return;
    }
    var panels = Array.prototype.slice.call(drawer.querySelectorAll("[data-batch-panel]"));
    var resources = Array.prototype.slice.call(drawer.querySelectorAll("[data-batch-resource]"));

    function selectedIDs() {
      return cards.filter(function (card) {
        var box = card.querySelector('input[type="checkbox"]');
        return box !== null && box.checked;
      }).map(function (card) {
        return card.querySelector('input[type="checkbox"]').value;
      });
    }

    function refresh() {
      var selected = selectedIDs();
      if (count !== null) {
        count.textContent = "已选 " + selected.length + " 台";
      }
      bar.hidden = selected.length === 0;
      drawer.querySelectorAll("[data-batch-button-disabled]").forEach(function (element) {
        element.disabled = selected.length === 0;
      });
    }

    cards.forEach(function (card) {
      var box = card.querySelector('input[type="checkbox"]');
      if (box !== null) {
        box.addEventListener("change", refresh);
      }
    });

    bar.querySelectorAll("[data-batch-button]").forEach(function (button) {
      button.addEventListener("click", function () {
        panels.forEach(function (panel) {
          panel.hidden = panel.getAttribute("data-batch-panel") !== button.getAttribute("data-batch-button");
        });
        drawer.hidden = false;
      });
    });

    drawer.querySelectorAll("[data-close-batch]").forEach(function (button) {
      button.addEventListener("click", function () {
        drawer.hidden = true;
      });
    });

    resources.forEach(function (link) {
      link.addEventListener("click", function (event) {
        event.preventDefault();
        var target = new URL(link.getAttribute("href"), window.location.origin);
        selectedIDs().forEach(function (id) {
          target.searchParams.append("server", id);
        });
        window.location.href = target.pathname + target.search;
      });
    });
    refresh();
  }

  // 服务器分步新增：地址行、同机识别、认证方式切换、地点搜索与 Profile
  //可见数都在浏览器内实时更新；写入仍由表单提交给服务器。
  function initServerWizard(root) {
    var addressList = root.querySelector("[data-address-list]");
    var addressTemplate = addressList === null ? null : addressList.querySelector("[data-address-row]");
    if (addressTemplate !== null) {
      var removeButton = addressTemplate.querySelector("[data-remove-address]");
      if (removeButton !== null) {
        removeButton.hidden = true;
      }
      root.querySelectorAll("[data-remove-address]").forEach(function (button) {
        button.addEventListener("click", function () {
          var row = button.closest("[data-address-row]");
          if (row !== null && row.parentNode !== null && row.parentNode.children.length > 1) {
            row.remove();
            detectSameMachine();
          }
        });
      });
      var addAddress = root.querySelector('button[name="add_address"]');
      if (addAddress !== null) {
        addAddress.addEventListener("click", function (event) {
          event.preventDefault();
          var row = addressTemplate.cloneNode(true);
          row.querySelectorAll("input, select").forEach(function (input) {
            if (input.type === "select-one") {
              input.value = "other";
            } else {
              input.value = "";
            }
          });
          var button = row.querySelector("[data-remove-address]");
          if (button !== null) {
            button.hidden = false;
          }
          addressList.appendChild(row);
        });
      }
    }

    var machineData = root.getAttribute("data-machine-addresses") || "";
    var machinesByAddress = {};
    machineData.split(";").forEach(function (item) {
      var parts = item.split("=");
      if (parts.length === 2 && parts[0] !== "" && parts[1] !== "") {
        machinesByAddress[parts[0].toLowerCase()] = parts[1];
      }
    });
    var samePanel = root.querySelector("[data-same-machine]");
    var sameName = root.querySelector("[data-same-machine-name]");
    var sameField = root.querySelector("[data-same-machine-field]");
    function detectSameMachine() {
      if (samePanel === null) {
        return;
      }
      var found = "";
      root.querySelectorAll("[data-address-input]").forEach(function (input) {
        var address = input.value.trim().toLowerCase();
        if (address !== "" && machinesByAddress[address] !== undefined) {
          found = machinesByAddress[address];
        }
      });
      samePanel.hidden = found === "";
      if (found !== "") {
        if (sameName !== null) {
          sameName.textContent = found;
        }
        if (sameField !== null) {
          sameField.value = found;
        }
      }
    }
    root.addEventListener("input", function (event) {
      if (event.target.hasAttribute && event.target.hasAttribute("data-address-input")) {
        detectSameMachine();
      }
    });
    detectSameMachine();

    var keyPasteButton = root.querySelector("[data-toggle-paste-key]");
    var keyPaste = root.querySelector("[data-paste-key]");
    var keySource = root.querySelector('input[name="key_source"]');
    if (keyPasteButton !== null && keyPaste !== null) {
      keyPasteButton.addEventListener("click", function () {
        keyPaste.hidden = false;
        if (keySource !== null) {
          keySource.value = "paste";
        }
        keyPasteButton.hidden = true;
      });
    }

    var credentialRadios = Array.prototype.slice.call(root.querySelectorAll('input[name="credential_action"]'));
    var keyPanel = root.querySelector("[data-credential-key]");
    var passwordPanel = root.querySelector("[data-credential-password]");
    credentialRadios.forEach(function (radio) {
      radio.addEventListener("change", function () {
        if (keyPanel !== null) {
          keyPanel.hidden = radio.value !== "key" || !radio.checked;
        }
        if (passwordPanel !== null) {
          passwordPanel.hidden = radio.value !== "password" || !radio.checked;
        }
      });
    });

    var siteSearch = root.querySelector("[data-site-search]");
    if (siteSearch !== null) {
      siteSearch.addEventListener("input", function () {
        var keyword = siteSearch.value.trim().toLowerCase();
        root.querySelectorAll("[data-site-row]").forEach(function (row) {
          row.hidden = keyword !== "" && row.getAttribute("data-site-text").toLowerCase().indexOf(keyword) < 0;
        });
      });
    }

    var profileStep = root.querySelector("[data-profile-step]");
    if (profileStep !== null) {
      var summary = profileStep.querySelector("[data-profile-client-summary]");
      function updateProfileVisibility() {
        var clients = {};
        profileStep.querySelectorAll('input[name="profile_id"]').forEach(function (box) {
          if (!box.checked) {
            return;
          }
          (box.getAttribute("data-client-ids") || "").split(",").forEach(function (id) {
            if (id !== "") {
              clients[id] = true;
            }
          });
        });
        if (summary !== null) {
          summary.textContent = "当前勾选将对 " + Object.keys(clients).length + " 个客户端可见";
        }
      }
      profileStep.querySelectorAll('input[name="profile_id"]').forEach(function (box) {
        box.addEventListener("change", updateProfileVisibility);
      });
      updateProfileVisibility();
    }
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
  document.querySelectorAll("[data-server-list-page]").forEach(initServerListBatch);
  document.querySelectorAll("[data-server-wizard]").forEach(initServerWizard);
  document.querySelectorAll("[data-server-selection]").forEach(initServerSelection);
})();
