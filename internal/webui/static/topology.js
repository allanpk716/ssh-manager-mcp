// 网络拓扑页面脚本：读取内部只读 JSON 接口，用 Cytoscape.js 呈现物理视
// 图、虚拟视图与叠加视图。本票只读，拖动只影响本次浏览，不保存布局。
(function () {
  "use strict";

  var container = document.getElementById("topology-canvas");
  if (container === null) {
    return;
  }
  var page = container.closest("[data-topology-page]");
  var detailPanel = page === null ? null : page.querySelector(".topology-detail");
  var trayList = page === null ? null : page.querySelector(".topology-tray-list");
  var trayEmpty = page === null ? null : page.querySelector(".topology-tray-empty");
  var viewButtons = page === null ? [] : Array.prototype.slice.call(page.querySelectorAll(".topology-view"));

  var state = { data: null, cy: null, view: "physical" };
  var coarsePointer = window.matchMedia("(pointer: coarse)");

  function escapeHTML(value) {
    return String(value === null || value === undefined ? "" : value)
      .replace(/&/g, "&amp;")
      .replace(/</g, "&lt;")
      .replace(/>/g, "&gt;")
      .replace(/"/g, "&quot;")
      .replace(/'/g, "&#39;");
  }

  function showError(message) {
    var existing = page === null ? null : page.querySelector(".topology-error");
    if (existing !== null) {
      existing.textContent = message;
      return;
    }
    var element = document.createElement("p");
    element.className = "topology-error";
    element.setAttribute("role", "alert");
    element.textContent = message;
    if (container.parentNode !== null) {
      container.parentNode.appendChild(element);
    }
  }

  function networkLabel(source) {
    return source === "physical" ? "共同物理局域网" : source === "virtual" ? "共同虚拟局域网" : "兜底主机";
  }

  function kindLabel(kind) {
    if (kind === "server") {
      return "关联服务器条目";
    }
    if (kind === "client") {
      return "关联客户端实例";
    }
    return "不受管设备";
  }

  function typeLabel(type) {
    return type === "physical" ? "物理局域网" : "虚拟局域网";
  }

  function indexData(data) {
    var networks = {};
    var devices = {};
    var membershipsByDevice = {};
    var membershipsByNetwork = {};
    var physicalBySite = {};
    var i;
    for (i = 0; i < data.networks.length; i += 1) {
      var network = data.networks[i];
      networks[network.id] = network;
      if (network.type === "physical") {
        physicalBySite[network.site_id] = physicalBySite[network.site_id] || [];
        physicalBySite[network.site_id].push(network);
      }
    }
    for (i = 0; i < data.devices.length; i += 1) {
      devices[data.devices[i].id] = data.devices[i];
    }
    for (i = 0; i < data.memberships.length; i += 1) {
      var membership = data.memberships[i];
      membershipsByDevice[membership.device_id] = membershipsByDevice[membership.device_id] || [];
      membershipsByDevice[membership.device_id].push(membership);
      membershipsByNetwork[membership.network_id] = membershipsByNetwork[membership.network_id] || [];
      membershipsByNetwork[membership.network_id].push(membership);
    }
    Object.keys(membershipsByDevice).forEach(function (deviceID) {
      membershipsByDevice[deviceID].sort(function (a, b) {
        return a.registration_order - b.registration_order;
      });
    });
    return {
      networks: networks,
      devices: devices,
      membershipsByDevice: membershipsByDevice,
      membershipsByNetwork: membershipsByNetwork,
      physicalBySite: physicalBySite,
    };
  }

  function physicalMembership(index, deviceID) {
    var memberships = index.membershipsByDevice[deviceID] || [];
    var physical = memberships.filter(function (membership) {
      return index.networks[membership.network_id] !== undefined && index.networks[membership.network_id].type === "physical";
    });
    physical.sort(function (a, b) {
      return a.registration_order - b.registration_order;
    });
    return physical;
  }

  function addressText(index, device) {
    var memberships = index.membershipsByDevice[device.id] || [];
    var addresses = memberships.map(function (membership) {
      return membership.address;
    });
    return addresses.slice(0, 2).join("、");
  }

  function kindGlyph(kind) {
    if (kind === "server") {
      return "▣ ";
    }
    if (kind === "client") {
      return "◍ ";
    }
    return "⬡ ";
  }

  function deviceNode(index, device, parentID, extraClasses) {
    var classes = ["device", "device-" + device.kind];
    if (state.data.local_device.known && state.data.local_device.device_id === device.id) {
      classes.push("local");
    }
    if (extraClasses !== undefined) {
      classes = classes.concat(extraClasses);
    }
    var label = kindGlyph(device.kind) + device.name;
    var addresses = addressText(index, device);
    if (addresses !== "") {
      label += "\n" + addresses;
    }
    if (device.servers.length > 1) {
      label += "\n×" + device.servers.length;
    }
    return {
      group: "nodes",
      data: { id: "device:" + device.id, label: label, parent: parentID, kind: device.kind, device: device.id },
      classes: classes.join(" "),
    };
  }

  function placeholderNode(parentID, label) {
    return {
      group: "nodes",
      data: { id: "empty:" + parentID, label: label, parent: parentID, placeholder: true },
      classes: "placeholder",
      selectable: false,
      grabbable: false,
      locked: true,
    };
  }

  function siteNode(site, virtualStyle) {
    return {
      group: "nodes",
      data: { id: "site:" + site.id, label: site.name, site: site.id },
      classes: virtualStyle ? "site site-label" : "site",
    };
  }

  function networkNode(network, parentID) {
    return {
      group: "nodes",
      data: {
        id: "network:" + network.id,
        label: network.name,
        parent: parentID === undefined ? undefined : parentID,
        network: network.id,
        networkType: network.type,
      },
      classes: "network network-" + network.type,
    };
  }

  function elementsForView(index, view) {
    var elements = [];
    var deviceIDs = {};
    var i;
    var j;

    if (view === "virtual") {
      var virtualNetworks = state.data.networks.filter(function (network) {
        return network.type === "virtual";
      });
      for (i = 0; i < state.data.sites.length; i += 1) {
        elements.push(siteNode(state.data.sites[i], true));
      }
      for (i = 0; i < virtualNetworks.length; i += 1) {
        var network = virtualNetworks[i];
        elements.push(networkNode(network));
        var members = index.membershipsByNetwork[network.id] || [];
        if (members.length === 0) {
          elements.push(placeholderNode("network:" + network.id, "暂无成员"));
        }
        var deviceSeen = {};
        for (j = 0; j < members.length; j += 1) {
          var device = index.devices[members[j].device_id];
          if (device === undefined || deviceSeen[device.id]) {
            continue;
          }
          deviceSeen[device.id] = true;
          var memberships = index.membershipsByDevice[device.id].filter(function (membership) {
            return index.networks[membership.network_id].type === "virtual";
          });
          if (memberships.length > 1) {
            elements.push(deviceNode(index, device, undefined));
            deviceIDs[device.id] = true;
            elements.push({
              group: "nodes",
              data: {
                id: "clone:" + network.id + ":" + device.id,
                label: kindGlyph(device.kind) + device.name + "\n" + members[j].address,
                parent: "network:" + network.id,
                kind: device.kind,
                device: device.id,
                clone: true,
              },
              classes: "device device-" + device.kind + " clone",
            });
            elements.push({
              group: "edges",
              data: {
                id: "clone-edge:" + network.id + ":" + device.id,
                source: "device:" + device.id,
                target: "clone:" + network.id + ":" + device.id,
              },
              classes: "clone-edge",
            });
          } else {
            elements.push(deviceNode(index, device, "network:" + network.id));
          }
          deviceIDs[device.id] = true;
        }
      }
      for (i = 0; i < state.data.devices.length; i += 1) {
        var remaining = state.data.devices[i];
        var virtualMemberships = (index.membershipsByDevice[remaining.id] || []).filter(function (membership) {
          return index.networks[membership.network_id].type === "virtual";
        });
        if (virtualMemberships.length === 0 && physicalMembership(index, remaining.id).length > 0) {
          elements.push(deviceNode(index, remaining));
          deviceIDs[remaining.id] = true;
        }
      }
      return elements;
    }

    for (i = 0; i < state.data.sites.length; i += 1) {
      var site = state.data.sites[i];
      elements.push(siteNode(site, false));
      var physicalNetworks = index.physicalBySite[site.id] || [];
      if (physicalNetworks.length === 0) {
        elements.push(placeholderNode("site:" + site.id, "暂无物理局域网"));
      }
      for (j = 0; j < physicalNetworks.length; j += 1) {
        var physical = physicalNetworks[j];
        elements.push(networkNode(physical, "site:" + site.id));
        var physicalMembers = index.membershipsByNetwork[physical.id] || [];
        if (physicalMembers.length === 0) {
          elements.push(placeholderNode("network:" + physical.id, "暂无成员"));
        }
      }
    }
    for (i = 0; i < state.data.networks.length; i += 1) {
      var candidate = state.data.networks[i];
      if (candidate.type !== "physical") {
        continue;
      }
      var candidateMembers = index.membershipsByNetwork[candidate.id] || [];
      for (j = 0; j < candidateMembers.length; j += 1) {
        var memberDevice = index.devices[candidateMembers[j].device_id];
        if (memberDevice === undefined || deviceIDs[memberDevice.id]) {
          continue;
        }
        deviceIDs[memberDevice.id] = true;
        var membershipsForDevice = physicalMembership(index, memberDevice.id);
        var firstNetwork = membershipsForDevice.length > 0 ? membershipsForDevice[0].network_id : candidate.id;
        elements.push(deviceNode(index, memberDevice, "network:" + firstNetwork));
        for (var k = 1; k < membershipsForDevice.length; k += 1) {
          if (membershipsForDevice[k].network_id === firstNetwork) {
            continue;
          }
          elements.push({
            group: "edges",
            data: {
              id: "member-edge:" + memberDevice.id + ":" + membershipsForDevice[k].network_id,
              source: "device:" + memberDevice.id,
              target: "network:" + membershipsForDevice[k].network_id,
            },
            classes: "member-edge",
          });
        }
      }
    }

    if (view === "overlay") {
      var virtualNetworksOverlay = state.data.networks.filter(function (network) {
        return network.type === "virtual";
      });
      for (i = 0; i < virtualNetworksOverlay.length; i += 1) {
        var overlayNetwork = virtualNetworksOverlay[i];
        elements.push({
          group: "nodes",
          data: {
            id: "vnet:" + overlayNetwork.id,
            label: overlayNetwork.name,
            network: overlayNetwork.id,
            networkType: "virtual",
          },
          classes: "network network-virtual virtual-hub",
        });
        var overlayMembers = index.membershipsByNetwork[overlayNetwork.id] || [];
        for (j = 0; j < overlayMembers.length; j += 1) {
          if (index.devices[overlayMembers[j].device_id] === undefined || !deviceIDs[overlayMembers[j].device_id]) {
            continue;
          }
          elements.push({
            group: "edges",
            data: {
              id: "virtual-edge:" + overlayNetwork.id + ":" + overlayMembers[j].device_id,
              source: "vnet:" + overlayNetwork.id,
              target: "device:" + overlayMembers[j].device_id,
            },
            classes: "virtual-edge",
          });
        }
      }
    }
    return elements;
  }

  function savedCoordinate(deviceID) {
    var coordinates = state.data.layout_coordinates || [];
    for (var i = 0; i < coordinates.length; i += 1) {
      if (coordinates[i].view_key === state.view && coordinates[i].device_id === deviceID) {
        return { x: coordinates[i].x, y: coordinates[i].y };
      }
    }
    return null;
  }

  function targetPositions(index, elements, view) {
    var positions = {};
    var perRow = 3;
    var deviceWidth = 190;
    var deviceHeight = 100;
    var i;
    var row;
    var column;

    if (view === "virtual") {
      var virtualNetworks = state.data.networks.filter(function (network) {
        return network.type === "virtual";
      });
      var baseX = 220;
      var baseY = 60;
      var rowCount = 0;
      for (i = 0; i < virtualNetworks.length; i += 1) {
        var members = index.membershipsByNetwork[virtualNetworks[i].id] || [];
        var uniqueDevices = [];
        var seen = {};
        var j;
        for (j = 0; j < members.length; j += 1) {
          if (!seen[members[j].device_id] && index.devices[members[j].device_id] !== undefined) {
            seen[members[j].device_id] = true;
            uniqueDevices.push(members[j].device_id);
          }
        }
        for (j = 0; j < uniqueDevices.length; j += 1) {
          row = Math.floor(j / perRow);
          column = j % perRow;
          positions["clone:" + virtualNetworks[i].id + ":" + uniqueDevices[j]] = {
            x: baseX + column * deviceWidth,
            y: baseY + rowCount * deviceHeight,
          };
          positions["device:" + uniqueDevices[j]] = positions["device:" + uniqueDevices[j]] || {
            x: baseX + column * deviceWidth + 20,
            y: baseY + rowCount * deviceHeight + 220,
          };
        }
        var saved = null;
        for (j = 0; j < uniqueDevices.length; j += 1) {
          saved = savedCoordinate(uniqueDevices[j]);
          if (saved !== null) {
            positions["device:" + uniqueDevices[j]] = saved;
          }
        }
        rowCount += 4;
      }
      var extraDevices = elements
        .map(function (element) {
          return element.data.id !== undefined && element.data.id.indexOf("device:") === 0 ? element.data.id : null;
        })
        .filter(function (id, offset, list) {
          return id !== null && list.indexOf(id) === offset;
        });
      for (i = 0; i < state.data.sites.length; i += 1) {
        positions["site:" + state.data.sites[i].id] = { x: 20, y: 80 + i * 140 };
      }
      for (i = 0; i < extraDevices.length; i += 1) {
        if (positions[extraDevices[i]] === undefined) {
          row = Math.floor(i / perRow);
          column = i % perRow;
          positions[extraDevices[i]] = { x: 220 + column * deviceWidth, y: 60 + row * deviceHeight };
        }
      }
      return positions;
    }

    var siteCount = state.data.sites.length;
    for (i = 0; i < siteCount; i += 1) {
      var siteX = 80 + i * 760;
      var siteY = 80;
      var physicalNetworks = index.physicalBySite[state.data.sites[i].id] || [];
      for (var n = 0; n < physicalNetworks.length; n += 1) {
        var membersPhysical = index.membershipsByNetwork[physicalNetworks[n].id] || [];
        for (var m = 0; m < membersPhysical.length; m += 1) {
          if (index.devices[membersPhysical[m].device_id] === undefined) {
            continue;
          }
          row = Math.floor(m / perRow);
          column = m % perRow;
          positions["device:" + membersPhysical[m].device_id] = {
            x: siteX + column * deviceWidth,
            y: siteY + n * (deviceHeight + 120) + row * deviceHeight,
          };
        }
      }
    }
    for (i = 0; i < state.data.devices.length; i += 1) {
      var savedPosition = savedCoordinate(state.data.devices[i].id);
      if (savedPosition !== null) {
        positions["device:" + state.data.devices[i].id] = savedPosition;
      }
    }

    if (view === "overlay") {
      var overlayNetworks = state.data.networks.filter(function (network) {
        return network.type === "virtual";
      });
      for (i = 0; i < overlayNetworks.length; i += 1) {
        positions["vnet:" + overlayNetworks[i].id] = { x: 120 + i * 240, y: 40 };
      }
    }
    return positions;
  }

  function styleForView() {
    return [
      { selector: "node", style: { "label": "data(label)", "text-valign": "center", "text-halign": "center", "font-size": 12, color: "#1f2937" } },
      { selector: ".site", style: { shape: "round-rectangle", "background-color": "#e8edf5", "border-color": "#98a2b3", "border-width": 2, "font-size": 15, "font-weight": "bold", padding: 28 } },
      { selector: ".site-label", style: { "background-color": "#f1f3f7", "border-color": "#d0d5dd", "border-style": "dashed", "font-size": 13, padding: 10 } },
      { selector: ".network", style: { shape: "round-rectangle", "background-color": "#f4f7fb", "border-color": "#84a6e0", "border-width": 2, padding: 22 } },
      { selector: ".network-virtual", style: { "background-color": "#f6f0fb", "border-color": "#a685e8", "border-style": "dashed" } },
      { selector: ".virtual-hub", style: { shape: "ellipse", width: 130, height: 58, "background-color": "#f1e7fb" } },
      { selector: ".device", style: { shape: "round-rectangle", width: 150, height: 64, "background-color": "#ffffff", "border-color": "#667085", "border-width": 1.5 } },
      { selector: ".device-server", style: { shape: "round-rectangle", "border-color": "#2f6fed", "border-width": 2 } },
      { selector: ".device-client", style: { shape: "ellipse", "border-color": "#129985", "border-width": 2 } },
      { selector: ".device-unmanaged", style: { shape: "hexagon", "border-color": "#7a5af8" } },
      { selector: ".local", style: { "border-color": "#f79009", "border-width": 4 } },
      { selector: ".clone", style: { "border-style": "dashed", opacity: 0.9 } },
      { selector: ".placeholder", style: { shape: "round-rectangle", "background-color": "#f9fafb", "border-color": "#d0d5dd", "border-style": "dashed", color: "#667085", width: 110, height: 40 } },
      { selector: "edge", style: { width: 2, "line-color": "#98a2b3" } },
      { selector: ".clone-edge", style: { "line-style": "dashed", "line-color": "#a685e8" } },
      { selector: ".virtual-edge", style: { "line-color": "#a685e8", "target-arrow-shape": "none" } },
      { selector: ".member-edge", style: { "line-style": "dotted", "line-color": "#84a6e0" } },
    ];
  }

  function applyView(view) {
    state.view = view;
    container.setAttribute("data-view", view);
    viewButtons.forEach(function (button) {
      button.classList.toggle("active", button.getAttribute("data-view") === view);
    });
    if (state.cy === null) {
      return;
    }
    var index = indexData(state.data);
    var oldPositions = {};
    state.cy.nodes().forEach(function (node) {
      oldPositions[node.id()] = node.position();
    });
    var elements = elementsForView(index, view);
    state.cy.json({ elements: [] });
    state.cy.add(elements);
    var positions = targetPositions(index, elements, view);
    state.cy.nodes().forEach(function (node) {
      var target = positions[node.id()] || { x: 0, y: 0 };
      var old = oldPositions[node.id()];
      if (old !== undefined && !node.isParent()) {
        node.position({ x: old.x, y: old.y });
      } else {
        node.position(target);
      }
    });
    state.cy
      .nodes()
      .filter(function (node) {
        return !node.isParent();
      })
      .animate(
        {
          position: function (node) {
            return positions[node.id()] || node.position();
          },
        },
        { duration: 320, easing: "ease-in-out-cubic" }
      );
    state.cy.fit(undefined, 48);
    updateCanvasSummary();
  }

  // updateCanvasSummary 把画布当前可见的节点摘要写进 data 属性与无障碍标
  // 签；画布本体由 Cytoscape 绘制，浏览器测试与屏幕阅读器需要这里的中继。
  function updateCanvasSummary() {
    if (state.cy === null) {
      return;
    }
    var deviceCount = 0;
    var cloneCount = 0;
    var labels = [];
    state.cy.nodes().forEach(function (node) {
      if (node.hasClass("placeholder") || node.isParent()) {
        return;
      }
      var label = node.data("label") || "";
      if (node.hasClass("clone")) {
        cloneCount += 1;
      } else if (node.data("device") !== undefined) {
        deviceCount += 1;
      }
      labels.push(label.split("\n").join(" "));
    });
    container.setAttribute("data-node-count", String(deviceCount));
    container.setAttribute("data-clone-count", String(cloneCount));
    var summary = labels.length === 0 ? "画布暂无节点" : labels.join("；");
    container.setAttribute("data-node-summary", summary);
    container.setAttribute("aria-label", "网络拓扑画布，当前" + state.view + "视图：" + summary);
  }

  function updateGrabbable() {
    if (state.cy === null) {
      return;
    }
    var canGrab = !coarsePointer.matches;
    container.setAttribute("data-grabbable", canGrab ? "true" : "false");
    state.cy.nodes().forEach(function (node) {
      node.grabbable(canGrab && !node.data("placeholder"));
    });
  }

  // suppressCytoscapeInlineStylesheet 预放一个同名标记节点，让 Cytoscape.js
  // 3.35 跳过它在页面头里插入内联样式表的动作；同等规则已写进 topology.css，
  // 内容安全策略因此保持只允许同源样式。
  function suppressCytoscapeInlineStylesheet() {
    var markerID = "__________cytoscape_stylesheet";
    if (document.getElementById(markerID) !== null) {
      return;
    }
    var marker = document.createElement("meta");
    marker.id = markerID;
    document.head.appendChild(marker);
  }

  function initGraph(data) {
    state.data = data;
    if (typeof window.cytoscape !== "function") {
      showError("网络拓扑画布组件加载失败。请刷新页面重试。");
      return;
    }
    var index = indexData(data);
    suppressCytoscapeInlineStylesheet();
    state.cy = window.cytoscape({
      container: container,
      elements: elementsForView(index, state.view),
      style: styleForView(),
      layout: { name: "preset" },
    });
    var positions = targetPositions(index, state.cy.elements().jsons(), state.view);
    state.cy.nodes().forEach(function (node) {
      var target = positions[node.id()] || { x: 0, y: 0 };
      if (!node.isParent()) {
        node.position(target);
      }
    });
    updateGrabbable();
    coarsePointer.addEventListener === undefined ? null : coarsePointer.addEventListener("change", updateGrabbable);
    state.cy.on("tap", "node", function (event) {
      var node = event.target;
      var deviceID = node.data("device");
      var networkID = node.data("network");
      var siteID = node.data("site");
      if (deviceID !== undefined) {
        showDeviceDetail(deviceID);
      } else if (networkID !== undefined) {
        showNetworkDetail(networkID);
      } else if (siteID !== undefined) {
        showSiteDetail(siteID);
      }
    });
    state.cy.on("tap", function (event) {
      if (event.target === state.cy) {
        hideDetail();
      }
    });
    state.cy.fit(undefined, 48);
  }

  function renderTray(index) {
    if (trayList === null) {
      return;
    }
    trayList.textContent = "";
    var tray = trayList.closest(".topology-tray");
    var unassigned = state.data.devices.filter(function (device) {
      return physicalMembership(index, device.id).length === 0;
    });
    if (tray !== null) {
      tray.setAttribute("data-count", String(unassigned.length));
    }
    trayEmpty.hidden = unassigned.length !== 0;
    unassigned.forEach(function (device) {
      var item = document.createElement("li");
      var button = document.createElement("button");
      button.type = "button";
      button.className = "topology-tray-item";
      button.setAttribute("data-device", device.id);
      var name = document.createElement("strong");
      name.textContent = device.name;
      var kind = document.createElement("span");
      kind.className = "tray-kind";
      kind.textContent = kindLabel(device.kind);
      button.appendChild(name);
      button.appendChild(kind);
      var addresses = addressText(index, device);
      if (addresses !== "") {
        kind.textContent += " · " + addresses;
      }
      item.appendChild(button);
      trayList.appendChild(item);
    });
  }

  function detailOpen(open) {
    if (detailPanel === null) {
      return;
    }
    detailPanel.hidden = !open;
    if (page !== null) {
      page.querySelector(".topology-body").classList.toggle("detail-open", open);
    }
  }

  function hideDetail() {
    detailOpen(false);
  }

  function showDetailHTML(html) {
    if (detailPanel === null) {
      return;
    }
    detailPanel.innerHTML = html;
    detailOpen(true);
  }

  function showDeviceDetail(deviceID) {
    var index = indexData(state.data);
    var device = index.devices[deviceID];
    if (device === undefined) {
      return;
    }
    var isLocal = state.data.local_device.known && state.data.local_device.device_id === device.id;
    var html = '<button type="button" class="detail-close">关闭</button>';
    html += "<h2>" + escapeHTML(device.name);
    if (isLocal) {
      html += '<span class="local-badge">本机</span>';
    }
    html += "</h2>";
    html += "<dl>";
    html += "<dt>设备类型</dt><dd>" + escapeHTML(kindLabel(device.kind)) + "</dd>";
    if (device.note !== "") {
      html += "<dt>备注</dt><dd>" + escapeHTML(device.note) + "</dd>";
    }
    if (device.client_name !== "") {
      html += "<dt>客户端实例</dt><dd>" + escapeHTML(device.client_name) + "</dd>";
    }
    html += "</dl>";

    html += "<h3>网络成员</h3><ul>";
    var memberships = index.membershipsByDevice[device.id] || [];
    if (memberships.length === 0) {
      html += "<li>尚未加入任何网络。</li>";
    }
    memberships.forEach(function (membership) {
      var network = index.networks[membership.network_id];
      html +=
        "<li>" +
        escapeHTML(network.name) +
        "（" +
        escapeHTML(typeLabel(network.type)) +
        "）· " +
        escapeHTML(membership.address) +
        "</li>";
    });
    html += "</ul>";

    html += "<h3>服务器条目</h3><ul>";
    if (device.servers.length === 0) {
      html += "<li>没有关联的服务器条目。</li>";
    }
    device.servers.forEach(function (server) {
      html +=
        '<li><a href="/ui/servers/' +
        encodeURIComponent(server.id) +
        '">' +
        escapeHTML(server.name) +
        "</a> · " +
        escapeHTML(server.user) +
        " · 端口 " +
        escapeHTML(server.port) +
        " · " +
        (server.pinned ? "已锚定" : "未锚定") +
        "</li>";
    });
    html += "</ul>";

    html += "<h3>候选 SSH 地址（本机起点）</h3>";
    if (device.candidate_notice !== "") {
      html += '<p class="candidate-notice">' + escapeHTML(device.candidate_notice) + "</p>";
    }
    html += "<ul>";
    if (device.candidates.length === 0) {
      html += "<li>没有可显示的候选地址。</li>";
    }
    device.candidates.forEach(function (candidate) {
      var source = networkLabel(candidate.source);
      if (candidate.network_name !== undefined && candidate.network_name !== "") {
        source += "：" + candidate.network_name;
      }
      if (candidate.server_name !== undefined && candidate.server_name !== "") {
        source += "：" + candidate.server_name;
      }
      html +=
        "<li>" + escapeHTML(candidate.address) + ' <span class="candidate-source">' + escapeHTML(source) + "</span></li>";
    });
    html += "</ul>";

    if (device.shared_storages.length > 0) {
      html += "<h3>提供的共享存储</h3><ul>";
      device.shared_storages.forEach(function (share) {
        html += "<li>" + escapeHTML(share.name) + "</li>";
      });
      html += "</ul>";
    }
    showDetailHTML(html);
  }

  function showSiteDetail(siteID) {
    var index = indexData(state.data);
    var site = null;
    state.data.sites.forEach(function (candidate) {
      if (candidate.id === siteID) {
        site = candidate;
      }
    });
    if (site === null) {
      return;
    }
    var networks = index.physicalBySite[site.id] || [];
    var html = '<button type="button" class="detail-close">关闭</button>';
    html += "<h2>" + escapeHTML(site.name) + "</h2><dl>";
    html += "<dt>类型</dt><dd>站点</dd>";
    if (site.note !== "") {
      html += "<dt>备注</dt><dd>" + escapeHTML(site.note) + "</dd>";
    }
    html += "</dl><h3>物理局域网</h3><ul>";
    if (networks.length === 0) {
      html += "<li>暂无物理局域网。</li>";
    }
    networks.forEach(function (network) {
      var count = (index.membershipsByNetwork[network.id] || []).length;
      html += "<li>" + escapeHTML(network.name) + " · " + count + " 台成员设备</li>";
    });
    html += "</ul>";
    showDetailHTML(html);
  }

  function showNetworkDetail(networkID) {
    var index = indexData(state.data);
    var network = index.networks[networkID];
    if (network === undefined) {
      return;
    }
    var members = index.membershipsByNetwork[network.id] || [];
    var html = '<button type="button" class="detail-close">关闭</button>';
    html += "<h2>" + escapeHTML(network.name) + "</h2><dl>";
    html += "<dt>类型</dt><dd>" + escapeHTML(typeLabel(network.type)) + "</dd>";
    if (network.type === "physical") {
      html += "<dt>所属站点</dt><dd>" + escapeHTML(network.site_name) + "</dd>";
    }
    if (network.network_range !== "") {
      html += "<dt>网段</dt><dd>" + escapeHTML(network.network_range) + "</dd>";
    }
    if (network.bandwidth_mbps > 0) {
      html += "<dt>标称带宽</dt><dd>" + escapeHTML(network.bandwidth_mbps) + " Mbit/s</dd>";
    }
    html += "<dt>按流量计费</dt><dd>" + (network.metered ? "是" : "否") + "</dd>";
    if (network.note !== "") {
      html += "<dt>备注</dt><dd>" + escapeHTML(network.note) + "</dd>";
    }
    html += "</dl><h3>成员</h3><ul>";
    if (members.length === 0) {
      html += "<li>暂无成员设备。</li>";
    }
    members.forEach(function (membership) {
      var device = index.devices[membership.device_id];
      var name = device === undefined ? membership.device_id : device.name;
      html += "<li>" + escapeHTML(name) + " · " + escapeHTML(membership.address) + "</li>";
    });
    html += "</ul>";
    showDetailHTML(html);
  }

  function bindUI() {
    viewButtons.forEach(function (button) {
      button.addEventListener("click", function () {
        applyView(button.getAttribute("data-view"));
      });
    });
    if (page !== null) {
      // 拓扑图数据在画布里，页面外的脚本或测试可以用这三个事件请求打开
      // 详情面板，与在画布里点节点等价。
      page.addEventListener("topology:select-device", function (event) {
        var detail = event.detail || {};
        state.data.devices.forEach(function (device) {
          if (device.name === detail.name || device.id === detail.id) {
            showDeviceDetail(device.id);
          }
        });
      });
      page.addEventListener("topology:select-network", function (event) {
        var detail = event.detail || {};
        state.data.networks.forEach(function (network) {
          if (network.name === detail.name || network.id === detail.id) {
            showNetworkDetail(network.id);
          }
        });
      });
      page.addEventListener("topology:select-site", function (event) {
        var detail = event.detail || {};
        state.data.sites.forEach(function (site) {
          if (site.name === detail.name || site.id === detail.id) {
            showSiteDetail(site.id);
          }
        });
      });
    }
    if (page !== null) {
      page.addEventListener("click", function (event) {
        var target = event.target;
        if (target instanceof Element && target.closest(".detail-close") !== null) {
          hideDetail();
          return;
        }
        var trayButton = target instanceof Element ? target.closest(".topology-tray-item") : null;
        if (trayButton !== null) {
          showDeviceDetail(trayButton.getAttribute("data-device"));
        }
      });
    }
  }

  function load() {
    fetch("/ui/api/topology", { headers: { Accept: "application/json" }, credentials: "same-origin" })
      .then(function (response) {
        if (!response.ok) {
          throw new Error("读取网络拓扑失败（" + response.status + "）。");
        }
        return response.json();
      })
      .then(function (data) {
        initGraph(data);
        renderTray(indexData(data));
        applyView(state.view);
      })
      .catch(function () {
        showError("读取网络拓扑失败。请刷新页面重试。");
      });
  }

  bindUI();
  load();
})();
