// 网络拓扑页面脚本：读取内部只读 JSON 接口，用 Cytoscape.js 呈现物理视
// 图、虚拟视图与叠加视图。可编辑角色拖动结束后把该视图的全部坐标保存到
// 权威端；客户端与手机只读查看，不提供保存入口。
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
  var statusElement = page === null ? null : page.querySelector("[data-topology-status]");
  var resetButton = page === null ? null : page.querySelector("[data-topology-reset]");

  var state = { data: null, cy: null, view: "physical", savedBeforeDrag: null, saving: false };
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

  // canEditLayout 判断当前角色与指针类型是否允许改布局：客户端只读，手机
  // 不能拖动；重置按钮同样遵循这两个限制。
  function canEditLayout() {
    if (page === null) {
      return false;
    }
    return page.getAttribute("data-layout-editable") === "true" && !coarsePointer.matches;
  }

  function csrfToken() {
    var meta = document.querySelector('meta[name="csrf-token"]');
    return meta === null ? "" : meta.getAttribute("content") || "";
  }

  function showLayoutStatus(message, isError) {
    if (statusElement === null) {
      return;
    }
    statusElement.textContent = message;
    statusElement.classList.toggle("error", isError === true);
    statusElement.hidden = message === "";
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

  function savedContainerCoordinate(entityType, entityID) {
    var coordinates = state.data.container_layout_coordinates || [];
    for (var i = 0; i < coordinates.length; i += 1) {
      if (coordinates[i].view_key === state.view &&
        coordinates[i].entity_type === entityType && coordinates[i].entity_id === entityID) {
        return { x: coordinates[i].x, y: coordinates[i].y };
      }
    }
    return null;
  }

  function applySavedContainerPositions(positions) {
    var i;
    for (i = 0; i < state.data.sites.length; i += 1) {
      var savedSite = savedContainerCoordinate("site", state.data.sites[i].id);
      if (savedSite !== null) {
        positions["site:" + state.data.sites[i].id] = savedSite;
      }
    }
    for (i = 0; i < state.data.networks.length; i += 1) {
      var network = state.data.networks[i];
      var savedNetwork = savedContainerCoordinate("network", network.id);
      if (savedNetwork === null) {
        continue;
      }
      if (positions["network:" + network.id] !== undefined) {
        positions["network:" + network.id] = savedNetwork;
      }
      if (positions["vnet:" + network.id] !== undefined) {
        positions["vnet:" + network.id] = savedNetwork;
      }
    }
    return positions;
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
      return applySavedContainerPositions(positions);
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
    return applySavedContainerPositions(positions);
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
      updateLayoutAttribute();
      return;
    }
    var index = indexData(state.data);
    var elements = elementsForView(index, view);
    // 每个视图的坐标各自保存；切换时不携带上一个视图的位置，重画后从当
    // 前位置动画过渡到本视图的保存坐标或自动排布。
    var previousPositions = {};
    state.cy.nodes().forEach(function (node) {
      var previous = node.position();
      previousPositions[node.id()] = { x: previous.x, y: previous.y };
    });
    state.cy.json({ elements: [] });
    state.cy.add(elements);
    var positions = targetPositions(index, elements, view);
    state.cy.nodes().forEach(function (node) {
      if (node.data("placeholder") || node.isParent()) {
        return;
      }
      var target = positions[node.id()] || { x: 0, y: 0 };
      var previous = previousPositions[node.id()];
      if (previous !== undefined) {
        node.position({ x: previous.x, y: previous.y });
      } else {
        node.position(target);
      }
    });
    // Cytoscape.js 的集合动画不支持按元素返回位置的函数；逐个节点给出目
    // 标坐标，过渡动画才能真正把节点带到本视图的保存坐标或自动排布。
    state.cy
      .nodes()
      .filter(function (node) {
        return !node.isParent() && !node.data("placeholder");
      })
      .forEach(function (node) {
        var target = positions[node.id()];
        if (target === undefined) {
          return;
        }
        node.animate(
          { position: { x: target.x, y: target.y } },
          { duration: 320, easing: "ease-in-out-cubic" }
        );
      });
    state.cy.fit(undefined, 48);
    updateCanvasSummary();
    updateLayoutAttribute();
    // 过渡动画结束后再写一次布局摘要，页面外读到的是落定后的坐标。
    window.setTimeout(updateLayoutAttribute, 360);
  }

  // currentLayout 把当前视图里可见的设备与容器坐标整理成一次保存载荷。
  // 分身不保存：同一台设备的本体节点是这个视图里的唯一坐标来源。
  function currentLayout() {
    var devices = [];
    var containers = [];
    if (state.cy !== null) {
      state.cy.nodes().forEach(function (node) {
        var data = node.data();
        if (data.placeholder) {
          return;
        }
        var position = node.position();
        if (data.device !== undefined && !data.clone) {
          devices.push({ device_id: data.device, x: position.x, y: position.y });
          return;
        }
        if (data.site !== undefined) {
          // 复合容器渲染出的框由子节点决定；保存框的中心而不是节点内部
          // 的存储坐标，避免子节点移动后留下过期值。
          var box = node.boundingBox();
          containers.push({
            entity_type: "site", entity_id: data.site,
            x: (box.x1 + box.x2) / 2, y: (box.y1 + box.y2) / 2,
          });
          return;
        }
        if (data.network !== undefined &&
          (node.id().indexOf("network:") === 0 || node.id().indexOf("vnet:") === 0)) {
          var networkBox = node.boundingBox();
          containers.push({
            entity_type: "network", entity_id: data.network,
            x: (networkBox.x1 + networkBox.x2) / 2, y: (networkBox.y1 + networkBox.y2) / 2,
          });
        }
      });
    }
    devices.sort(function (a, b) { return a.device_id < b.device_id ? -1 : 1; });
    containers.sort(function (a, b) {
      if (a.entity_type !== b.entity_type) {
        return a.entity_type < b.entity_type ? -1 : 1;
      }
      return a.entity_id < b.entity_id ? -1 : 1;
    });
    return { view: state.view, devices: devices, containers: containers };
  }

  // updateLayoutAttribute 把当前布局写进 data 属性；浏览器测试用它断言保存
  // 与重绘后的坐标，不接触画布内部对象。
  function updateLayoutAttribute() {
    if (state.data === null) {
      return;
    }
    container.setAttribute("data-layout-json", JSON.stringify(currentLayout()));
  }

  // rememberSavedLayout 在保存成功后同步本地数据，让切换视图再切回来时仍
  // 看到刚保存的坐标。
  function rememberSavedLayout(layout) {
    state.data.layout_coordinates = (state.data.layout_coordinates || []).filter(function (coordinate) {
      return coordinate.view_key !== layout.view;
    });
    state.data.container_layout_coordinates = (state.data.container_layout_coordinates || []).filter(
      function (coordinate) {
        return coordinate.view_key !== layout.view;
      }
    );
    layout.devices.forEach(function (coordinate) {
      state.data.layout_coordinates.push({
        view_key: layout.view, device_id: coordinate.device_id, x: coordinate.x, y: coordinate.y,
      });
    });
    layout.containers.forEach(function (coordinate) {
      state.data.container_layout_coordinates.push({
        view_key: layout.view, entity_type: coordinate.entity_type,
        entity_id: coordinate.entity_id, x: coordinate.x, y: coordinate.y,
      });
    });
  }

  function forgetSavedLayout(view) {
    state.data.layout_coordinates = (state.data.layout_coordinates || []).filter(function (coordinate) {
      return coordinate.view_key !== view;
    });
    state.data.container_layout_coordinates = (state.data.container_layout_coordinates || []).filter(
      function (coordinate) {
        return coordinate.view_key !== view;
      }
    );
  }

  // saveCurrentLayout 一次提交当前视图的全部坐标。保存进行中不重复发起；
  // 失败时保留画布现状并提示，下一次拖动会再试一次。
  function saveCurrentLayout() {
    if (!canEditLayout() || state.saving) {
      return;
    }
    var layout = currentLayout();
    state.saving = true;
    fetch("/ui/api/topology/layout", {
      method: "POST",
      headers: { "Content-Type": "application/json", Accept: "application/json", "X-CSRF-Token": csrfToken() },
      credentials: "same-origin",
      body: JSON.stringify(layout),
    })
      .then(function (response) {
        if (response.status === 204) {
          return null;
        }
        return response.json().catch(function () {
          return { error: "保存布局失败，请稍后重试。" };
        });
      })
      .then(function (payload) {
        state.saving = false;
        if (payload === null) {
          rememberSavedLayout(layout);
          updateLayoutAttribute();
          showLayoutStatus("布局已保存。", false);
          return;
        }
        showLayoutStatus(payload.error || "保存布局失败，请稍后重试。", true);
      })
      .catch(function () {
        state.saving = false;
        showLayoutStatus("保存布局失败，请检查网络后重试。", true);
      });
  }

  // resetCurrentLayout 先确认再清空当前视图保存的坐标，随后按自动排布重画。
  function resetCurrentLayout() {
    if (!canEditLayout() || state.data === null) {
      return;
    }
    var viewLabel = state.view === "physical" ? "物理视图" : state.view === "virtual" ? "虚拟视图" : "叠加视图";
    if (!window.confirm("确定要重置" + viewLabel + "的布局吗？该视图保存的全部坐标会被清除。")) {
      return;
    }
    fetch("/ui/api/topology/layout/reset", {
      method: "POST",
      headers: { "Content-Type": "application/json", Accept: "application/json", "X-CSRF-Token": csrfToken() },
      credentials: "same-origin",
      body: JSON.stringify({ view: state.view }),
    })
      .then(function (response) {
        if (response.status === 204) {
          return null;
        }
        return response.json().catch(function () {
          return { error: "重置布局失败，请稍后重试。" };
        });
      })
      .then(function (payload) {
        if (payload === null) {
          forgetSavedLayout(state.view);
          applyView(state.view);
          showLayoutStatus("布局已重置。", false);
          return;
        }
        showLayoutStatus(payload.error || "重置布局失败，请稍后重试。", true);
      })
      .catch(function () {
        showLayoutStatus("重置布局失败，请检查网络后重试。", true);
      });
  }

  // isContainerNode 判断节点是否是站点区域或网络分组框；重叠检查只对这两
  // 类实体生效。
  function isContainerNode(node) {
    var data = node.data();
    return data.placeholder !== true &&
      ((data.site !== undefined && node.id().indexOf("site:") === 0) ||
        (data.network !== undefined &&
          (node.id().indexOf("network:") === 0 || node.id().indexOf("vnet:") === 0)));
  }

  // overlappingContainer 返回与指定容器明显重叠的另一个容器节点；交叠面积
  // 超过较小框面积的四分之一视为明显重叠。嵌套关系不算重叠：网络分组框
  // 本来就画在所属站点区域里。
  function overlappingContainer(node) {
    if (state.cy === null || !isContainerNode(node)) {
      return null;
    }
    var box = node.boundingBox();
    var containers = state.cy.nodes().filter(function (candidate) {
      if (candidate.id() === node.id() || !isContainerNode(candidate)) {
        return false;
      }
      // 只要一方是另一方的祖先，就是嵌套而不是区域重叠。
      if (candidate.ancestors().contains(node) || node.ancestors().contains(candidate)) {
        return false;
      }
      return true;
    });
    var found = null;
    containers.forEach(function (candidate) {
      if (found !== null) {
        return;
      }
      var other = candidate.boundingBox();
      var width = Math.min(box.x2, other.x2) - Math.max(box.x1, other.x1);
      var height = Math.min(box.y2, other.y2) - Math.max(box.y1, other.y1);
      if (width <= 0 || height <= 0) {
        return;
      }
      var intersection = width * height;
      var areaA = (box.x2 - box.x1) * (box.y2 - box.y1);
      var areaB = (other.x2 - other.x1) * (other.y2 - other.y1);
      if (intersection > Math.min(areaA, areaB) * 0.25) {
        found = candidate;
      }
    });
    return found;
  }

  // restorePositions 把整幅画布恢复到拖动前的坐标；站点或网络拖动时子节点
  // 会一起移动，只还原容器本身不够。
  function restorePositions(positions) {
    if (state.cy === null || positions === null) {
      return;
    }
    state.cy.nodes().forEach(function (node) {
      var target = positions[node.id()];
      // 只还原子节点：复合容器的框由子节点决定，直接设置父节点会把子
      // 树再平移一次。先停掉未完成的过渡动画，避免动画把还原结果改掉。
      if (target !== undefined && !node.data("placeholder") && !node.isParent()) {
        node.stop();
        node.position({ x: target.x, y: target.y });
      }
    });
  }

  function snapshotPositions() {
    var positions = {};
    if (state.cy === null) {
      return positions;
    }
    state.cy.nodes().forEach(function (node) {
      // position() 返回的是节点内部坐标对象的引用；必须复制，否则节点移
      // 动后这份「拖动前快照」会跟着变成新坐标。
      var position = node.position();
      positions[node.id()] = { x: position.x, y: position.y };
    });
    return positions;
  }

  // moveEntityTo 是页面外脚本与浏览器测试的拖动接缝：把一个实体移动到指定
  // 坐标，并走与手动拖动相同的保存路径。
  function moveEntityTo(entityType, entityID, x, y) {
    if (state.cy === null || !canEditLayout()) {
      return;
    }
    var prefixes = entityType === "site"
      ? ["site:"]
      : entityType === "network"
        ? ["network:", "vnet:"]
        : ["device:"];
    var node = null;
    for (var i = 0; i < prefixes.length; i += 1) {
      var candidate = state.cy.getElementById(prefixes[i] + entityID);
      if (!candidate.empty()) {
        node = candidate;
        break;
      }
    }
    if (node === null) {
      return;
    }
    // 先停掉视图切换可能还没完成的过渡动画，再记下拖动前坐标；否则动画
    // 会把新位置改回旧目标。
    if (node.isParent()) {
      node.descendants().forEach(function (child) {
        child.stop();
      });
    }
    node.stop();
    state.savedBeforeDrag = snapshotPositions();
    if (node.isParent()) {
      // 复合容器的框由子节点决定：把整个子树平移，让框中心落在目标坐
      // 标，与手动拖动容器时的整体移动一致。
      var box = node.boundingBox();
      var deltaX = x - (box.x1 + box.x2) / 2;
      var deltaY = y - (box.y1 + box.y2) / 2;
      // 给复合容器设置坐标会把整个子树平移同样的差值。
      var stored = node.position();
      node.position({ x: stored.x + deltaX, y: stored.y + deltaY });
    } else {
      node.position({ x: x, y: y });
    }
    var overlap = overlappingContainer(node);
    if (overlap !== null) {
      restorePositions(state.savedBeforeDrag);
      updateLayoutAttribute();
      showLayoutStatus("区域与其他区域明显重叠，已还原本次移动。", true);
      return;
    }
    updateLayoutAttribute();
    saveCurrentLayout();
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
    var canEdit = canEditLayout();
    if (resetButton !== null) {
      resetButton.hidden = !canEdit;
    }
    if (state.cy === null) {
      container.setAttribute("data-grabbable", canEdit ? "true" : "false");
      return;
    }
    container.setAttribute("data-grabbable", canEdit ? "true" : "false");
    state.cy.nodes().forEach(function (node) {
      node.grabbable(canEdit && !node.data("placeholder"));
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
      var target = positions[node.id()];
      if (node.data("placeholder") || target === undefined) {
        return;
      }
      // 复合容器的框由子节点决定，子节点的保存坐标已经还原了整个子树的
      // 位置；单独定位父节点会把子树再推一次。
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
    // 拖动开始先记下整幅画布坐标：容器明显重叠时要整体还原，设备拖动失
    // 败时也不丢其它节点的位置。
    state.cy.on("grab", "node", function () {
      state.savedBeforeDrag = snapshotPositions();
    });
    state.cy.on("dragfree", "node", function (event) {
      if (!canEditLayout()) {
        return;
      }
      var node = event.target;
      node.stop();
      var overlap = overlappingContainer(node);
      if (overlap !== null) {
        restorePositions(state.savedBeforeDrag);
        updateLayoutAttribute();
        showLayoutStatus("区域与其他区域明显重叠，已还原本次拖动。", true);
        return;
      }
      updateLayoutAttribute();
      saveCurrentLayout();
    });
    state.cy.fit(undefined, 48);
    updateLayoutAttribute();
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
    if (resetButton !== null) {
      resetButton.addEventListener("click", resetCurrentLayout);
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
      // 页面外的脚本或测试可以用这个事件请求移动一个实体，与在画布里拖动
      // 等价；只读角色与手机视口忽略它。
      page.addEventListener("topology:move", function (event) {
        var detail = event.detail || {};
        moveEntityTo(detail.entity_type, detail.entity_id, detail.x, detail.y);
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
