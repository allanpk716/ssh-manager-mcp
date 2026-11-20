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
        target.searchParams.set("mode", "add");
        target.searchParams.set("return_to", "/ui/servers/");
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

  // 服务器详情地址弹层：加行、删行与「其他地址最多一行」都在浏览器内即
  // 时更新；最终校验与写入仍由服务器处理整份表单。
  function initServerDetailAddressEditor(root) {
    var list = root.querySelector("[data-address-list]");
    var template = list === null ? null : list.querySelector("[data-address-row]");
    if (template === null) {
      return;
    }
    var clone = template.cloneNode(true);

    function refresh() {
      var selects = list.querySelectorAll("[data-address-network]");
      var otherSelected = 0;
      selects.forEach(function (select) {
        if (select.value === "other") {
          otherSelected += 1;
        }
      });
      selects.forEach(function (select, index) {
        var rowNumber = index + 1;
        var row = select.closest("[data-address-row]");
        select.setAttribute("aria-label", "第" + rowNumber + "行网络");
        var label = select.parentNode.querySelector("span");
        if (label !== null) {
          label.textContent = "第" + rowNumber + "行网络";
        }
        var input = row === null ? null : row.querySelector("[data-address-input]");
        if (input !== null) {
          input.setAttribute("aria-label", "第" + rowNumber + "行地址");
          var inputLabel = input.parentNode.querySelector("span");
          if (inputLabel !== null) {
            inputLabel.textContent = "第" + rowNumber + "行地址";
          }
        }
        var remove = row === null ? null : row.querySelector("[data-remove-address]");
        if (remove !== null) {
          remove.setAttribute("aria-label", "删除第" + rowNumber + "行地址");
        }
        var otherOption = select.querySelector('option[value="other"]');
        if (otherOption !== null) {
          otherOption.disabled = otherSelected > 0 && select.value !== "other";
        }
      });
    }

    root.querySelectorAll("[data-remove-address]").forEach(function (button) {
      button.addEventListener("click", function () {
        var row = button.closest("[data-address-row]");
        if (row !== null && row.parentNode !== null) {
          row.remove();
          refresh();
        }
      });
    });
    var addButton = root.querySelector("[data-add-detail-address]");
    if (addButton !== null) {
      addButton.addEventListener("click", function (event) {
        event.preventDefault();
        var row = clone.cloneNode(true);
        var select = row.querySelector("[data-address-network]");
        var input = row.querySelector("[data-address-input]");
        if (input !== null) {
          input.value = "";
        }
        if (select !== null) {
          var chosen = null;
          select.querySelectorAll("option").forEach(function (option) {
            if (chosen === null && !option.disabled && option.value !== "other") {
              chosen = option.value;
            }
          });
          select.value = chosen === null ? "other" : chosen;
        }
        list.appendChild(row);
        var remove = row.querySelector("[data-remove-address]");
        if (remove !== null) {
          remove.addEventListener("click", function () {
            row.remove();
            refresh();
          });
        }
        refresh();
      });
    }
    root.addEventListener("change", function (event) {
      if (event.target.hasAttribute && event.target.hasAttribute("data-address-network")) {
        refresh();
      }
    });
    refresh();
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
    var candidateToggle = event.target.closest("[data-connection-test-candidates]");
    if (candidateToggle !== null) {
      var candidateList = document.getElementById(candidateToggle.getAttribute("aria-controls"));
      var expanded = candidateToggle.getAttribute("aria-expanded") !== "true";
      candidateToggle.setAttribute("aria-expanded", expanded ? "true" : "false");
      candidateToggle.textContent = expanded ? "收起每个候选地址的结果" : "展开看每个候选地址的结果";
      if (candidateList !== null) {
        candidateList.hidden = !expanded;
      }
      return;
    }
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
  // 服务器详情分区编辑弹层：打开时聚焦第一个控件并限制焦点；Escape 与遮
  // 罩关闭；无弹层但带卡片锚点时把焦点还给对应卡片的编辑按钮。
  function initServerDetailDrawer() {
    function focusEditButtonForHash() {
      var anchorMatch = /^#server-card-([a-z]+)$/.exec(window.location.hash);
      if (anchorMatch === null) {
        return;
      }
      var editButton = document.querySelector(
        '[data-detail-edit-button="' + anchorMatch[1] + '"]'
      );
      if (editButton !== null) {
        window.requestAnimationFrame(function () {
          editButton.focus();
        });
      }
    }

    window.addEventListener("hashchange", focusEditButtonForHash);
    var shell = document.querySelector("[data-server-detail-drawer]");
    if (shell === null) {
      focusEditButtonForHash();
      return;
    }
    var dialog = shell.querySelector("[data-server-detail-dialog]");
    if (dialog === null) {
      return;
    }
    function focusableElements() {
      return Array.prototype.slice.call(
        dialog.querySelectorAll(
          'a[href], button:not([disabled]), input:not([disabled]), select:not([disabled]), textarea:not([disabled])'
        )
      ).filter(function (element) {
        return element.offsetParent !== null || element === document.activeElement;
      });
    }
    var controls = dialog.querySelectorAll("input, select, textarea");
    var first = null;
    for (var index = 0; index < controls.length; index += 1) {
      var control = controls[index];
      if (control.type !== "hidden" && !control.disabled && control.offsetParent !== null) {
        first = control;
        break;
      }
    }
    if (first === null) {
      first = dialog.querySelector("a[href], button");
    }
    if (first !== null) {
      first.focus();
    }
    document.body.classList.add("modal-open");
    dialog.addEventListener("keydown", function (event) {
      if (event.key !== "Tab") {
        return;
      }
      var elements = focusableElements();
      if (elements.length === 0) {
        return;
      }
      var firstElement = elements[0];
      var lastElement = elements[elements.length - 1];
      if (event.shiftKey && document.activeElement === firstElement) {
        event.preventDefault();
        lastElement.focus();
      } else if (!event.shiftKey && document.activeElement === lastElement) {
        event.preventDefault();
        firstElement.focus();
      }
    });
    document.addEventListener("keydown", function (event) {
      if (event.key === "Escape") {
        window.location.assign(shell.getAttribute("data-close-detail-edit"));
      }
    });
  }

  initServerDetailDrawer();
  document
    .querySelectorAll("[data-server-detail-address-editor]")
    .forEach(initServerDetailAddressEditor);
  document.querySelectorAll("[data-server-list-page]").forEach(initServerListBatch);
  document.querySelectorAll("[data-server-wizard]").forEach(initServerWizard);
  document.querySelectorAll("[data-server-selection]").forEach(initServerSelection);
})();
