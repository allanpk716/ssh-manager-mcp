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

  // 服务器列表筛选：搜索、地点、网络与「只看需要注意」选完即提交；无脚
  // 本环境仍保留原生 GET 表单与提交按钮。
  function initServerListFilters(root) {
    var form = root.querySelector("[data-server-filter-form]");
    if (form === null) {
      return;
    }
    var search = form.querySelector('input[name="query"]');
    var attention = form.querySelector('input[name="attention"]');
    var selects = form.querySelector("[data-server-filter-selects]");
    var buttons = root.querySelector("[data-server-filter-buttons]");
    var noScriptSubmit = form.querySelector("[data-no-script-filter-submit]");
    var searchTimer = 0;

    function submitForm() {
      if (typeof form.requestSubmit === "function") {
        form.requestSubmit();
        return;
      }
      form.submit();
    }

    if (search !== null) {
      search.addEventListener("input", function () {
        window.clearTimeout(searchTimer);
        searchTimer = window.setTimeout(submitForm, 250);
      });
    }
    if (attention !== null) {
      attention.addEventListener("change", submitForm);
    }
    if (selects !== null && buttons !== null) {
      selects.hidden = true;
      buttons.hidden = false;
    }
    if (noScriptSubmit !== null) {
      noScriptSubmit.hidden = true;
    }

    var sheets = Array.prototype.slice.call(root.querySelectorAll("[data-server-filter-sheet]"));
    function closeSheets() {
      sheets.forEach(function (sheet) {
        sheet.hidden = true;
      });
      if (buttons !== null) {
        buttons.querySelectorAll("[data-open-server-filter]").forEach(function (button) {
          button.setAttribute("aria-expanded", "false");
        });
      }
    }

    if (buttons !== null) {
      buttons.querySelectorAll("[data-open-server-filter]").forEach(function (button) {
        button.addEventListener("click", function () {
          var sheet = document.getElementById(button.getAttribute("aria-controls"));
          closeSheets();
          if (sheet !== null) {
            sheet.hidden = false;
            button.setAttribute("aria-expanded", "true");
          }
        });
      });
      root.querySelectorAll("[data-close-server-filter]").forEach(function (element) {
        element.addEventListener("click", closeSheets);
      });
    }
    root.querySelectorAll("[data-server-filter-choice]").forEach(function (choice) {
      choice.addEventListener("click", function () {
        var field = form.querySelector('select[name="' + choice.getAttribute("data-server-filter-choice") + '"]');
        if (field !== null) {
          field.value = choice.getAttribute("data-filter-value");
        }
        closeSheets();
        submitForm();
      });
    });
    root.addEventListener("keydown", function (event) {
      if (event.key === "Escape") {
        closeSheets();
      }
    });
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

  // 批量测试连接：提交选中集合后立即把每张卡片换成进行中状态，之后逐
  // 秒读取每台任务的状态。卡片完成文案由服务器按服务器列表卡片既有的
  // 短状态规则生成，页面只负责替换显示，不另建第二套状态或文案。
  function initServerListBatchConnection(root, selected) {
    var bar = document.querySelector("[data-server-selection-bar]");
    var button = bar === null ? null : bar.querySelector("[data-server-test-connection-action]");
    if (button === null) {
      return { refresh: function () {} };
    }
    var path = "/ui/servers/batch/connection-test";
    var testing = false;
    var testingCount = 0;
    var startFailureCount = 0;
    var originalLines = {};
    var summary = null;

    function cardsById() {
      var cards = {};
      root.querySelectorAll("[data-server-card]").forEach(function (card) {
        var box = card.querySelector('input[type="checkbox"]');
        if (box !== null) {
          cards[box.value] = card;
        }
      });
      return cards;
    }

    function makeLine(state, title, detail, untested) {
      var line = document.createElement("p");
      line.className = untested
        ? "server-test-line is-untested"
        : state === "running"
        ? "server-test-line is-running"
        : state === "succeeded"
          ? "server-test-line is-succeeded"
          : "server-test-line is-failed";
      line.setAttribute("data-server-connection-status", "");
      var mark = document.createElement("span");
      mark.className = state === "running" ? "connection-test-spinner" : "status-dot";
      mark.setAttribute("aria-hidden", "true");
      var copy = document.createElement("span");
      copy.className = "server-test-copy";
      var heading = document.createElement("strong");
      heading.className = "server-test-title";
      heading.textContent = title;
      copy.appendChild(heading);
      if (detail) {
        var detailElement = document.createElement("span");
        detailElement.className = "server-test-detail";
        detailElement.textContent = detail;
        copy.appendChild(detailElement);
      }
      line.appendChild(mark);
      line.appendChild(copy);
      return line;
    }

    function replaceLine(card, line) {
      var old = card.querySelector("[data-server-connection-status]");
      if (old === null || old.parentNode === null) {
        return;
      }
      old.parentNode.replaceChild(line, old);
    }

    function showSummary(kind, message) {
      if (summary === null) {
        summary = document.createElement("div");
        summary.setAttribute("data-server-batch-connection-summary", "");
        summary.setAttribute("role", "status");
        summary.setAttribute("aria-live", "polite");
        var paragraph = document.createElement("p");
        summary.appendChild(paragraph);
        var list = root.querySelector(".server-card-list");
        if (list !== null && list.parentNode !== null) {
          list.parentNode.insertBefore(summary, list);
        } else {
          root.appendChild(summary);
        }
      }
      summary.className = "notice " + kind + " server-batch-connection-summary";
      summary.firstChild.textContent = message;
    }

    function refresh() {
      button.disabled = testing || selected.size === 0;
      button.textContent = testing ? "测试中 " + testingCount + " 台" : "测试连接";
    }

    function finish(message) {
      testing = false;
      testingCount = 0;
      refresh();
      if (message !== "") {
        showSummary("notice-error", message);
      }
    }

    function finishedSummary(body) {
      var failed = body.failed || 0;
      var message = "连接测试完成：" + ((body.total || 0) + startFailureCount) +
        " 台，已连上 " + (body.succeeded || 0) + " 台，连不上 " + failed + " 台。";
      if (startFailureCount > 0) {
        message += "另有 " + startFailureCount + " 台未能启动。";
      }
      showSummary(startFailureCount > 0 ? "notice-error" : "notice-info", message);
    }

    function restoreLines(ids, message) {
      var cards = cardsById();
      ids.forEach(function (id) {
        var saved = originalLines[id];
        if (saved !== undefined && cards[id] !== undefined) {
          replaceLine(cards[id], saved.cloneNode(true));
        }
      });
      finish(message);
    }

    function renderStatus(tasks, body) {
      var cards = cardsById();
      var serverByTask = {};
      tasks.forEach(function (task) {
        serverByTask[task.task_id] = task.server_id;
      });
      (body.cards || []).forEach(function (card) {
        var target = cards[card.server_id || serverByTask[card.task_id]];
        if (target === undefined) {
          return;
        }
        if (card.state === "running" || card.state === "succeeded" || card.state === "failed") {
        replaceLine(target, makeLine(card.state, card.title, card.detail, card.untested));
        }
      });
      if (!body.done) {
        window.setTimeout(function () {
          pollStatus(tasks);
        }, 1000);
        return;
      }
      testing = false;
      testingCount = 0;
      refresh();
      finishedSummary(body);
    }

    function pollStatus(tasks) {
      var params = new URLSearchParams();
      tasks.forEach(function (task) {
        params.append("task", task.task_id);
        params.append("server", task.server_id);
      });
      window
        .fetch(path + "?" + params.toString(), { headers: { Accept: "application/json" } })
        .then(function (response) {
          return response.json().then(function (body) {
            return { ok: response.ok, body: body };
          });
        })
        .then(function (result) {
          if (!result.ok) {
            throw new Error(result.body.error);
          }
          renderStatus(tasks, result.body);
        })
        .catch(function () {
          finish("读取连接测试状态失败，请刷新页面查看最新结果。");
        });
    }

    function start() {
      if (testing || selected.size === 0) {
        return;
      }
      var ids = [];
      selected.forEach(function (id) {
        ids.push(id);
      });
      var cards = cardsById();
      var usable = ids.every(function (id) {
        return cards[id] !== undefined && cards[id].querySelector("[data-server-connection-status]") !== null;
      });
      if (!usable) {
        showSummary("notice-error", "选中的服务器不在当前列表里，请刷新页面后重试。");
        return;
      }
      testing = true;
      testingCount = ids.length;
      startFailureCount = 0;
      originalLines = {};
      refresh();
      ids.forEach(function (id) {
        originalLines[id] = cards[id].querySelector("[data-server-connection-status]").cloneNode(true);
        replaceLine(cards[id], makeLine("running", "正在连接", ""));
      });
      var form = new URLSearchParams();
      ids.forEach(function (id) {
        form.append("server", id);
      });
      window
        .fetch(path, {
          method: "POST",
          headers: {
            "Content-Type": "application/x-www-form-urlencoded;charset=UTF-8",
            "X-CSRF-Token": token,
          },
          body: form.toString(),
        })
        .then(function (response) {
          return response.json().then(function (body) {
            return { ok: response.ok, body: body };
          });
        })
        .then(function (result) {
          if (!result.ok) {
            throw new Error(result.body.error);
          }
          var tasks = result.body.tasks || [];
          var pollTasks = [];
          var cards = cardsById();
          tasks.forEach(function (task) {
            var target = cards[task.server_id];
            if (task.error) {
              startFailureCount += 1;
              testingCount -= 1;
              if (target !== undefined) {
                replaceLine(target, makeLine("failed", "未能启动连接测试", task.error));
              }
              return;
            }
            pollTasks.push(task);
          });
          if (pollTasks.length === 0) {
            renderStatus([], {
              cards: [], done: true, total: 0, running: 0, succeeded: 0, failed: 0,
            });
            return;
          }
          pollStatus(pollTasks);
        })
        .catch(function (error) {
          var message = error && error.message ? error.message : "批量连接测试没有启动，请稍后重试。";
          restoreLines(ids, message);
        });
    }

    button.addEventListener("click", start);
    return { refresh: refresh };
  }

  // 服务器列表多选：进入选择模式后点卡片或复选框都能勾选，全选只取当
  // 前筛选结果。选择模式与选中集合保存在会话存储里，筛选整页刷新和批
  // 量授权往返后都能恢复；「更多 ⋯」里的资源链接把选中标识符以重复的
  // server 查询参数带到加入 profile、授权机密项与授权共享存储共用的服务
  // 器勾选页，写入仍由既有确认流程执行。
  function initServerListBatch(root, selected) {
    var multiButton = root.querySelector("[data-server-multi-select]");
    var multiLabel = multiButton === null ? null : multiButton.querySelector("[data-server-multi-select-label]");
    var cards = Array.prototype.slice.call(root.querySelectorAll("[data-server-card]"));
    var bar = document.querySelector("[data-server-selection-bar]");
    var moreDrawer = document.querySelector("[data-server-more-drawer]");
    var drawer = document.querySelector("[data-server-batch-drawer]");
    var count = document.querySelector("[data-server-selected-count]");
    if (multiButton === null || bar === null || drawer === null) {
      return;
    }
    var panels = Array.prototype.slice.call(drawer.querySelectorAll("[data-batch-panel]"));
    var resources = Array.prototype.slice.call(drawer.querySelectorAll("[data-batch-resource]"));
    var selectAllButton = bar.querySelector("[data-server-select-all]");
    var cancelButton = bar.querySelector("[data-server-cancel-selection]");
    var moreButton = bar.querySelector("[data-server-more-button]");
    var modeKey = "sshmgr-servers-selecting";
    var selectedKey = "sshmgr-servers-selected";
    var selecting = false;
    var connectionTests = initServerListBatchConnection(root, selected);

    function persist() {
      try {
        if (!selecting) {
          window.sessionStorage.removeItem(modeKey);
          window.sessionStorage.removeItem(selectedKey);
          return;
        }
        window.sessionStorage.setItem(modeKey, "1");
        window.sessionStorage.setItem(selectedKey, JSON.stringify(Array.from(selected)));
      } catch (error) {
        // 会话存储不可用时只影响跨页保留，本页内选择不受影响。
      }
    }

    function restore() {
      try {
        var saved = window.sessionStorage.getItem(selectedKey);
        var ids = saved === null ? [] : JSON.parse(saved);
        if (Array.isArray(ids)) {
          ids.forEach(function (id) {
            if (typeof id === "string" && id !== "") {
              selected.add(id);
            }
          });
        }
        return window.sessionStorage.getItem(modeKey) === "1";
      } catch (error) {
        return false;
      }
    }

    function trimSelectionToRenderedCards() {
      var renderedIDs = new Set();
      cards.forEach(function (card) {
        var box = card.querySelector('input[type="checkbox"]');
        if (box !== null) {
          renderedIDs.add(box.value);
        }
      });
      selected.forEach(function (id) {
        if (!renderedIDs.has(id)) {
          selected.delete(id);
        }
      });
    }

    function refresh() {
      cards.forEach(function (card) {
        var box = card.querySelector('input[type="checkbox"]');
        if (box !== null) {
          box.checked = selected.has(box.value);
        }
      });
      if (count !== null) {
        count.textContent = "已选 " + selected.size + " 台";
      }
      if (selectAllButton !== null) {
        selectAllButton.disabled = cards.length === 0;
      }
      if (moreButton !== null) {
        moreButton.disabled = selected.size === 0;
      }
      connectionTests.refresh();
      bar.hidden = !selecting;
      root.classList.toggle("server-selecting", selecting);
      document.body.classList.toggle("server-selecting", selecting);
      multiButton.setAttribute("aria-pressed", selecting ? "true" : "false");
      if (multiLabel !== null) {
        multiLabel.textContent = selecting ? "退出多选" : "多选";
      }
      bar.dispatchEvent(new CustomEvent("sshmgr-server-selection-change"));
      persist();
    }

    function setSelecting(active) {
      selecting = active;
      if (!active) {
        selected.clear();
      }
      refresh();
    }

    function toggleCard(card) {
      var box = card.querySelector('input[type="checkbox"]');
      if (box === null) {
        return;
      }
      if (selected.has(box.value)) {
        selected.delete(box.value);
      } else {
        selected.add(box.value);
      }
      refresh();
    }

    multiButton.addEventListener("click", function () {
      setSelecting(!selecting);
    });
    if (cancelButton !== null) {
      cancelButton.addEventListener("click", function () {
        setSelecting(false);
      });
    }
    if (selectAllButton !== null) {
      selectAllButton.addEventListener("click", function () {
        selected.clear();
        cards.forEach(function (card) {
          var box = card.querySelector('input[type="checkbox"]');
          if (box !== null) {
            selected.add(box.value);
          }
        });
        refresh();
      });
    }

    // 选择模式下点卡片（包括卡片里的链接）只切换勾选，不进入详情；复选
    // 框自己的点击交给原生行为，避免一次点击切换两次。
    root.addEventListener("click", function (event) {
      if (!selecting) {
        return;
      }
      var card = event.target.closest("[data-server-card]");
      if (card === null || event.target.closest(".server-select-control") !== null) {
        return;
      }
      event.preventDefault();
      toggleCard(card);
    });
    root.addEventListener("change", function (event) {
      if (!selecting) {
        return;
      }
      var card = event.target.closest("[data-server-card]");
      if (card === null || !event.target.matches('input[type="checkbox"]')) {
        return;
      }
      if (event.target.checked) {
        selected.add(event.target.value);
      } else {
        selected.delete(event.target.value);
      }
      refresh();
    });

    if (moreButton !== null && moreDrawer !== null) {
      moreButton.addEventListener("click", function () {
        moreDrawer.hidden = false;
      });
      moreDrawer.querySelectorAll("[data-close-more]").forEach(function (element) {
        element.addEventListener("click", function () {
          moreDrawer.hidden = true;
        });
      });
      moreDrawer.querySelectorAll("[data-batch-button]").forEach(function (button) {
        button.addEventListener("click", function () {
          moreDrawer.hidden = true;
          panels.forEach(function (panel) {
            panel.hidden = panel.getAttribute("data-batch-panel") !== button.getAttribute("data-batch-button");
          });
          drawer.hidden = false;
        });
      });
    }

    drawer.querySelectorAll("[data-close-batch]").forEach(function (button) {
      button.addEventListener("click", function () {
        drawer.hidden = true;
      });
    });
    document.addEventListener("keydown", function (event) {
      if (event.key === "Escape") {
        if (moreDrawer !== null) {
          moreDrawer.hidden = true;
        }
        drawer.hidden = true;
      }
    });

    resources.forEach(function (link) {
      link.addEventListener("click", function (event) {
        event.preventDefault();
        var target = new URL(link.getAttribute("href"), window.location.origin);
        selected.forEach(function (id) {
          target.searchParams.append("server", id);
        });
        target.searchParams.set("mode", "add");
        target.searchParams.set("return_to", window.location.pathname + window.location.search);
        window.location.href = target.pathname + target.search;
      });
    });
    selecting = restore();
    trimSelectionToRenderedCards();
    refresh();
  }

  // 服务器列表批量设置地点：底部操作条按钮打开地点树单选弹层；选完地点
  // 后带着选中服务器条目进入后果确认页，写入仍由确认页表单提交给服务
  // 器，浏览器端不做任何直接写入。
  function initServerBatchLocation(root, selected) {
    var button = document.querySelector("[data-server-set-location-action]");
    var bar = document.querySelector("[data-server-selection-bar]");
    var drawer = document.querySelector("[data-server-location-drawer]");
    if (button === null || bar === null || drawer === null) {
      return;
    }
    var next = drawer.querySelector("[data-server-location-next]");

    function chosenSite() {
      var checked = drawer.querySelector('input[name="batch_site"]:checked');
      return checked === null ? "" : checked.value;
    }

    function refresh() {
      button.disabled = selected.size === 0;
      if (next !== null) {
        next.disabled = chosenSite() === "";
      }
    }

    function close() {
      drawer.hidden = true;
    }

    button.addEventListener("click", function () {
      drawer.hidden = false;
    });
    drawer.querySelectorAll("[data-close-server-location]").forEach(function (element) {
      element.addEventListener("click", close);
    });
    drawer.querySelectorAll('input[name="batch_site"]').forEach(function (radio) {
      radio.addEventListener("change", refresh);
    });
    if (next !== null) {
      next.addEventListener("click", function () {
        var site = chosenSite();
        if (site === "" || selected.size === 0) {
          return;
        }
        var target = new URL("/ui/servers/batch/location", window.location.origin);
        selected.forEach(function (id) {
          target.searchParams.append("server", id);
        });
        target.searchParams.set("site", site);
        target.searchParams.set("return_to", window.location.pathname + window.location.search);
        window.location.href = target.pathname + target.search;
      });
    }
    bar.addEventListener("sshmgr-server-selection-change", refresh);
    document.addEventListener("keydown", function (event) {
      if (event.key === "Escape") {
        close();
      }
    });

    // 从后果确认页返回上一步时，带着 batch_location=1 重新打开弹层，并清
    // 掉这个参数，刷新后不会再次自动打开。
    var current = new URL(window.location.href);
    if (current.searchParams.get("batch_location") === "1") {
      current.searchParams.delete("batch_location");
      window.history.replaceState(null, "", current.pathname + (current.search === "" ? "" : current.search));
      drawer.hidden = false;
    }
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
  document.querySelectorAll("[data-server-list-page]").forEach(initServerListFilters);
  document.querySelectorAll("[data-server-list-page]").forEach(function (root) {
    var selected = new Set();
    initServerListBatch(root, selected);
    initServerBatchLocation(root, selected);
  });
  document.querySelectorAll("[data-server-wizard]").forEach(initServerWizard);
  document.querySelectorAll("[data-server-selection]").forEach(initServerSelection);
})();
