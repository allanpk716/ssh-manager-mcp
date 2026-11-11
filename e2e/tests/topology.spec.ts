import { expect, test } from "../helpers";
import * as net from "node:net";
import {
  openClientUI,
  startClientUIWithTopology,
  startStandaloneUIWithTopology,
} from "../helpers";

async function openTopologyPage(
  page: import("@playwright/test").Page,
  ui: { startupURL: string },
): Promise<void> {
  await page.goto(ui.startupURL);
  await page.getByRole("heading", { name: "服务器" }).waitFor();
  await page.getByRole("link", { name: "网络拓扑" }).click();
  await page.getByRole("heading", { name: "网络拓扑" }).waitFor();
}

async function startLocalProbeTarget(): Promise<net.Server> {
  const server = net.createServer((socket) => socket.end());
  await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", () => resolve()));
  return server;
}

async function selectTopologyEntity(
  page: import("@playwright/test").Page,
  event: string,
  name: string,
): Promise<void> {
  await page.evaluate(
    ({ eventName, entityName }) => {
      document
        .querySelector("[data-topology-page]")
        ?.dispatchEvent(new CustomEvent(eventName, { detail: { name: entityName }, bubbles: true }));
    },
    { eventName: event, entityName: name },
  );
}

type canvasPoint = { x: number; y: number };
type canvasBox = canvasPoint & { x1: number; y1: number; x2: number; y2: number };

// canvasNodeBox 把画布节点的渲染包围盒换算成页面坐标；拓扑脚本把
// Cytoscape.js 实例挂在页面元素上供浏览器测试使用。
async function canvasNodeBox(
  page: import("@playwright/test").Page,
  kind: "device" | "network" | "site",
  name: string,
): Promise<canvasBox> {
  return page.evaluate(
    ({ kind, entityName }) => {
      const pageEl = document.querySelector("[data-topology-page]") as any;
      const cy = pageEl.sshmgrTopology;
      if (!cy) {
        throw new Error("画布实例尚未就绪");
      }
      let found: any = null;
      cy.nodes().forEach((node: any) => {
        const label = String(node.data("label") || "");
        if (kind === "device") {
          if (node.data("device") !== undefined && !node.hasClass("clone") && label.split("\n")[0].includes(entityName)) {
            found = node;
          }
        } else if (kind === "network") {
          if (node.data("network") !== undefined && label === entityName) {
            found = node;
          }
        } else if (node.data("site") !== undefined && label === entityName) {
          found = node;
        }
      });
      if (found === null) {
        throw new Error(`画布上找不到${kind === "device" ? "设备" : kind === "network" ? "网络" : "站点"}：${entityName}`);
      }
      const box = found.renderedBoundingBox({ includeLabels: true, includeOverlays: false });
      const rect = document.getElementById("topology-canvas")!.getBoundingClientRect();
      return {
        x: rect.left + (box.x1 + box.x2) / 2,
        y: rect.top + (box.y1 + box.y2) / 2,
        x1: rect.left + box.x1,
        y1: rect.top + box.y1,
        x2: rect.left + box.x2,
        y2: rect.top + box.y2,
      };
    },
    { kind, entityName: name },
  );
}

// pointInSiteOutsideNetworks 在站点区域内找一个不在任何物理局域网框里的
// 投放点，覆盖「拖到站点但不在网络框」的流程。
async function pointInSiteOutsideNetworks(
  page: import("@playwright/test").Page,
  siteName: string,
): Promise<canvasPoint> {
  return page.evaluate((name) => {
    const pageEl = document.querySelector("[data-topology-page]") as any;
    const cy = pageEl.sshmgrTopology;
    const rect = document.getElementById("topology-canvas")!.getBoundingClientRect();
    let siteBox: { x1: number; y1: number; x2: number; y2: number } | null = null;
    const networkBoxes: Array<{ x1: number; y1: number; x2: number; y2: number }> = [];
    cy.nodes().forEach((node: any) => {
      const box = node.renderedBoundingBox({ includeLabels: true, includeOverlays: false });
      if (node.data("site") !== undefined && node.data("label") === name) {
        siteBox = box;
      }
      if (node.data("network") !== undefined) {
        networkBoxes.push(box);
      }
    });
    if (siteBox === null) {
      throw new Error(`画布上找不到站点：${name}`);
    }
    const inside = (x: number, y: number, box: { x1: number; y1: number; x2: number; y2: number }) =>
      x >= box.x1 && x <= box.x2 && y >= box.y1 && y <= box.y2;
    const fractions = [0.05, 0.3, 0.5, 0.7, 0.95];
    for (const fx of fractions) {
      for (const fy of fractions) {
        const x = siteBox!.x1 + (siteBox!.x2 - siteBox!.x1) * fx;
        const y = siteBox!.y1 + (siteBox!.y2 - siteBox!.y1) * fy;
        if (!networkBoxes.some((box) => inside(x, y, box))) {
          return { x: rect.left + x, y: rect.top + y };
        }
      }
    }
    throw new Error("站点区域内找不到物理局域网框之外的投放点");
  }, siteName);
}

// emptyCanvasPoint 返回画布里不被任何分组框占据的点，用于「拖出网络」。
async function emptyCanvasPoint(page: import("@playwright/test").Page): Promise<canvasPoint> {
  return page.evaluate(() => {
    const pageEl = document.querySelector("[data-topology-page]") as any;
    const cy = pageEl.sshmgrTopology;
    const rect = document.getElementById("topology-canvas")!.getBoundingClientRect();
    const boxes: Array<{ x1: number; y1: number; x2: number; y2: number }> = [];
    cy.nodes().forEach((node: any) => {
      if (node.data("network") !== undefined || node.data("site") !== undefined) {
        boxes.push(node.renderedBoundingBox({ includeLabels: true, includeOverlays: false }));
      }
    });
    const inside = (x: number, y: number, box: { x1: number; y1: number; x2: number; y2: number }) =>
      x >= box.x1 && x <= box.x2 && y >= box.y1 && y <= box.y2;
    for (let fx = 0.03; fx <= 0.97; fx += 0.02) {
      for (let fy = 0.03; fy <= 0.97; fy += 0.02) {
        const x = rect.width * fx;
        const y = rect.height * fy;
        if (!boxes.some((box) => inside(x, y, box))) {
          return { x: rect.left + x, y: rect.top + y };
        }
      }
    }
    throw new Error("画布里找不到空白投放点");
  });
}

async function dragByMouse(
  page: import("@playwright/test").Page,
  from: canvasPoint,
  to: canvasPoint,
): Promise<void> {
  await page.mouse.move(from.x, from.y);
  await page.mouse.down();
  await page.mouse.move(to.x, to.y, { steps: 16 });
  await page.mouse.up();
}

// expectMembershipRow 断言设备详情里的网络成员行；可编辑角色的地址在输
// 入框的值里，不在文本里。
async function expectMembershipRow(
  detail: import("@playwright/test").Locator,
  networkName: string,
  address?: string,
): Promise<void> {
  const row = detail.locator(".detail-member-form", { hasText: networkName + "（" });
  await expect(row).toHaveCount(1);
  if (address !== undefined) {
    await expect(row.locator('input[name="address"]')).toHaveValue(address);
  }
}

async function expectNoMembershipRow(
  detail: import("@playwright/test").Locator,
  networkName: string,
): Promise<void> {
  await expect(detail.locator(".detail-member-form", { hasText: networkName + "（" })).toHaveCount(0);
}

test("桌面端切换三视图并查看托盘、角标与详情", async ({ page, isMobile }) => {
  test.skip(isMobile, "桌面交互用例只在桌面视口运行");
  const ui = await startStandaloneUIWithTopology();
  const consoleErrors: string[] = [];
  page.on("console", (message) => {
    if (message.type() === "error") {
      consoleErrors.push(message.text());
    }
  });
  try {
    await openTopologyPage(page, ui);
    const canvas = page.locator("#topology-canvas");

    await expect(canvas).toHaveAttribute("data-view", "physical");
    await expect(canvas).toHaveAttribute("data-grabbable", "true");
    const physicalSummary = await canvas.getAttribute("data-node-summary");
    expect(physicalSummary).toContain("工作站");
    expect(physicalSummary).toContain("×2");

    const tray = page.getByRole("complementary", { name: "未归类设备" });
    await expect(tray).toHaveAttribute("data-count", "2");
    await expect(tray.getByRole("button", { name: /笔记本/ })).toBeVisible();
    await expect(tray.getByRole("button", { name: /old-1/ })).toBeVisible();

    await page.getByRole("button", { name: "虚拟视图" }).click();
    await expect(canvas).toHaveAttribute("data-view", "virtual");
    await expect(canvas).not.toHaveAttribute("data-clone-count", "0");
    await expect(canvas).toHaveAttribute("data-grabbable", "true");

    await page.getByRole("button", { name: "叠加视图" }).click();
    await expect(canvas).toHaveAttribute("data-view", "overlay");
    await expect(canvas).not.toHaveAttribute("data-node-count", "0");

    const detail = page.getByRole("complementary", { name: "拓扑详情" });
    await selectTopologyEntity(page, "topology:select-device", "工作站");
    await expect(detail).toBeVisible();
    await expect(detail).toContainText("本机");
    await expect(detail).toContainText("关联服务器条目");
    await expect(detail.getByRole("link", { name: "web-1" })).toBeVisible();
    await expect(detail.getByRole("link", { name: "db-1" })).toBeVisible();
    await expectMembershipRow(detail, "家A局域网", "192.168.1.20");
    await expect(detail).toContainText("共同物理局域网");
    await expect(detail.getByRole("button", { name: "关闭" })).toBeVisible();

    await selectTopologyEntity(page, "topology:select-network", "家A局域网");
    await expect(detail).toContainText("192.168.1.0/24");
    await expect(detail).toContainText("1000 Mbit/s");
    await expect(detail).toContainText("按流量计费是");

    await selectTopologyEntity(page, "topology:select-site", "家A");
    // 探测叠加用例给家A局域网补了一台「探测机」，成员数随之增加。
    await expect(detail).toContainText("家A局域网 · 3 台成员设备");
    expect(consoleErrors).toEqual([]);
  } finally {
    await ui.close();
  }
});

test("手机视口整幅查看并可从底部打开详情", async ({ page, isMobile }) => {
  test.skip(!isMobile, "手机视口用例只在手机视口运行");
  const ui = await startStandaloneUIWithTopology();
  try {
    await openTopologyPage(page, ui);
    const canvas = page.locator("#topology-canvas");
    await expect(canvas).toHaveAttribute("data-view", "physical");
    await expect(canvas).toHaveAttribute("data-grabbable", "false");
    await expect(canvas).not.toHaveAttribute("data-node-count", "0");
    const tray = page.getByRole("complementary", { name: "未归类设备" });
    await expect(tray).toHaveAttribute("data-draggable", "false");

    const overflow = await page.evaluate(
      () => document.documentElement.scrollWidth - document.documentElement.clientWidth,
    );
    expect(overflow).toBeLessThanOrEqual(1);

    await selectTopologyEntity(page, "topology:select-device", "NAS");
    const detail = page.getByRole("complementary", { name: "拓扑详情" });
    await expect(detail).toBeVisible();
    await expect(detail).toContainText("照片与备份");
    await expect(detail).toContainText("家NAS共享");
    const viewportHeight = page.viewportSize()?.height ?? 0;
    const box = await detail.boundingBox();
    expect(box).not.toBeNull();
    expect(box!.y + box!.height).toBeGreaterThan(viewportHeight - 80);
  } finally {
    await ui.close();
  }
});

test("客户端只读查看本地快照拓扑", async ({ page }) => {
  const ui = await startClientUIWithTopology();
  try {
    await openClientUI(page, ui);
    await page.getByRole("link", { name: "网络拓扑" }).click();
    await page.getByRole("heading", { name: "网络拓扑" }).waitFor();

    await expect(page.getByText("拓扑为只读视图；网络成员的修改请到权威端 Web UI 做。")).toBeVisible();
    const canvas = page.locator("#topology-canvas");
    await expect(canvas).not.toHaveAttribute("data-node-count", "0");
    await expect(canvas).toHaveAttribute("data-grabbable", "false");
    await expect(page.getByRole("complementary", { name: "未归类设备" })).toHaveAttribute("data-draggable", "false");
    const summary = await canvas.getAttribute("data-node-summary");
    expect(summary).toContain("客户端服务器设备");
    expect(summary).not.toContain("×2");

    const tray = page.getByRole("complementary", { name: "未归类设备" });
    await expect(tray).toHaveAttribute("data-count", "1");

    const detail = page.getByRole("complementary", { name: "拓扑详情" });
    await selectTopologyEntity(page, "topology:select-device", "客户端服务器设备");
    await expect(detail).toBeVisible();
    await expect(detail.getByRole("link", { name: "客户端服务器" })).toBeVisible();
    await expect(detail).toContainText("兜底主机");
    await expect(detail).toContainText("192.0.2.30");
    await expect(detail.getByRole("button", { name: "编辑" })).toHaveCount(0);

    const origin = page.getByLabel("探测起点");
    await expect(origin).toBeDisabled();
    await expect(origin.locator("option")).toHaveCount(1);
  } finally {
    await ui.close();
  }
});

test("桌面端叠加探测结果、切换起点并按需触发", async ({ page, isMobile }) => {
  test.skip(isMobile, "桌面交互用例只在桌面视口运行");
  const probeServer = await startLocalProbeTarget();
  const probePort = (probeServer.address() as net.AddressInfo).port;
  const ui = await startStandaloneUIWithTopology(probePort);
  const consoleErrors: string[] = [];
  page.on("console", (message) => {
    if (message.type() === "error") {
      consoleErrors.push(message.text());
    }
  });
  try {
    await openTopologyPage(page, ui);
    const canvas = page.locator("#topology-canvas");
    await expect(canvas).toHaveAttribute("data-probe-summary", /NAS：✓3ms/);
    const initialSummary = await canvas.getAttribute("data-probe-summary");
    expect(initialSummary).toContain("未探测");
    expect(initialSummary).toContain("无共同网络");

    const detail = page.getByRole("complementary", { name: "拓扑详情" });
    await selectTopologyEntity(page, "topology:select-device", "NAS");
    await expect(detail).toContainText("探测结果（当前起点：本机 工作站）");
    await expect(detail).toContainText("家A局域网 · 192.168.1.30:22 · 通（3 毫秒，刚刚）");
    await expect(detail).toContainText("虚拟网 · 100.64.0.30:22 · 不通（连接超时，刚刚）");

    const origin = page.getByLabel("探测起点");
    await origin.selectOption({ label: "gpu-1（机房机器）" });
    await expect(canvas).toHaveAttribute("data-probe-summary", /NAS：无共同网络/);
    await expect(detail).toContainText("无共同网络，未探测");

    await origin.selectOption({ label: "本机（工作站）" });
    await expect(canvas).toHaveAttribute("data-probe-summary", /NAS：✓3ms/);

    await selectTopologyEntity(page, "topology:select-device", "探测机");
    await detail.getByLabel("探测目标").selectOption({ label: `服务器 probe-target（端口 ${probePort}）` });
    await detail.getByRole("button", { name: "开始探测" }).click();
    const status = page.getByRole("status");
    await expect(status).toContainText("探测完成", { timeout: 15_000 });
    await expect(canvas).toHaveAttribute("data-probe-summary", /探测机：✓(\d+ms|<1ms)/);
    await expect(detail).toContainText(`127.0.0.1:${probePort} · 通（`);
    await expect(detail).toContainText(`127.0.0.2:${probePort} · 不通（`);
    expect(consoleErrors).toEqual([]);
  } finally {
    probeServer.close();
    await ui.close();
  }
});

test("桌面端托盘拖入网络框与站点内选网新建", async ({ page, isMobile }) => {
  test.skip(isMobile, "拖动归类用例只在桌面视口运行");
  const ui = await startStandaloneUIWithTopology();
  const consoleErrors: string[] = [];
  page.on("console", (message) => {
    if (message.type() === "error") {
      consoleErrors.push(message.text());
    }
  });
  try {
    await openTopologyPage(page, ui);
    const tray = page.getByRole("complementary", { name: "未归类设备" });
    await expect(tray).toHaveAttribute("data-draggable", "true");
    const dialog = page.locator("dialog.topology-dialog");

    // 托盘设备直接拖进物理局域网框：填地址后归类。
    const oldDevice = tray.getByRole("button", { name: /old-1/ });
    const oldBox = await oldDevice.boundingBox();
    expect(oldBox).not.toBeNull();
    const networkBox = await canvasNodeBox(page, "network", "机房局域网");
    await dragByMouse(page, { x: oldBox!.x + oldBox!.width / 2, y: oldBox!.y + 10 }, networkBox);
    await expect(dialog).toBeVisible();
    await expect(dialog).toContainText("old-1");
    await expect(dialog).toContainText("机房局域网");
    await dialog.locator('input[name="address"]').fill("10.0.0.21");
    await dialog.getByRole("button", { name: "加入网络" }).click();
    await expect(dialog).not.toBeVisible();
    await expect(tray).toHaveAttribute("data-count", "1");
    await selectTopologyEntity(page, "topology:select-network", "机房局域网");
    const detail = page.getByRole("complementary", { name: "拓扑详情" });
    await expect(detail).toContainText("old-1 · 10.0.0.21");
    await page.waitForTimeout(400);

    // 托盘设备拖到站点区域但不在网络框：选新建物理局域网。
    const laptop = tray.getByRole("button", { name: /笔记本/ });
    const laptopBox = await laptop.boundingBox();
    expect(laptopBox).not.toBeNull();
    const dropPoint = await pointInSiteOutsideNetworks(page, "机房");
    await dragByMouse(page, { x: laptopBox!.x + laptopBox!.width / 2, y: laptopBox!.y + 10 }, dropPoint);
    await expect(dialog).toBeVisible();
    await expect(dialog).toContainText("笔记本");
    await expect(dialog).toContainText("机房");
    await dialog.getByRole("radio", { name: /新建物理局域网/ }).check();
    await dialog.locator(".new-network-name input").fill("新机房局域网");
    await dialog.locator('input[name="address"]').fill("10.1.0.9");
    await dialog.getByRole("button", { name: "保存归类" }).click();
    await expect(dialog).not.toBeVisible();
    await expect(tray).toHaveAttribute("data-count", "0");
    await expect(tray.getByText("全部设备都已归入物理局域网。")).toBeVisible();
    await selectTopologyEntity(page, "topology:select-site", "机房");
    await expect(detail).toContainText("新机房局域网");
    expect(consoleErrors).toEqual([]);
  } finally {
    await ui.close();
  }
});

test("物理局域网之间拖动可选择移动或同时加入", async ({ page, isMobile }) => {
  test.skip(isMobile, "拖动归类用例只在桌面视口运行");
  const ui = await startStandaloneUIWithTopology();
  try {
    await openTopologyPage(page, ui);
    const dialog = page.locator("dialog.topology-dialog");
    const detail = page.getByRole("complementary", { name: "拓扑详情" });

    // 先选「移动」：原网络成员删除，目标网络登记新地址。
    let deviceBox = await canvasNodeBox(page, "device", "工作站");
    let target = await canvasNodeBox(page, "network", "机房局域网");
    await dragByMouse(page, deviceBox, target);
    await expect(dialog).toBeVisible();
    await expect(dialog).toContainText("归类方式");
    await dialog.getByRole("radio", { name: /移动/ }).check();
    await dialog.locator('input[name="address"]').fill("10.0.0.20");
    await dialog.getByRole("button", { name: "保存归类" }).click();
    await expect(dialog).not.toBeVisible();
    await selectTopologyEntity(page, "topology:select-device", "工作站");
    await expectMembershipRow(detail, "机房局域网", "10.0.0.20");
    await expectNoMembershipRow(detail, "家A局域网");
    await page.waitForTimeout(400);

    // 再选「同时加入」：保留原成员并新增目标网络地址。
    deviceBox = await canvasNodeBox(page, "device", "工作站");
    target = await canvasNodeBox(page, "network", "家A局域网");
    await dragByMouse(page, deviceBox, target);
    await expect(dialog).toBeVisible();
    await dialog.getByRole("radio", { name: /同时加入/ }).check();
    await dialog.locator('input[name="address"]').fill("192.168.1.29");
    await dialog.getByRole("button", { name: "保存归类" }).click();
    await expect(dialog).not.toBeVisible();
    await selectTopologyEntity(page, "topology:select-device", "工作站");
    await expectMembershipRow(detail, "家A局域网", "192.168.1.29");
    await expectMembershipRow(detail, "机房局域网", "10.0.0.20");
  } finally {
    await ui.close();
  }
});

test("拖回托盘先确认且取消保留原状态", async ({ page, isMobile }) => {
  test.skip(isMobile, "拖动归类用例只在桌面视口运行");
  const ui = await startStandaloneUIWithTopology();
  try {
    await openTopologyPage(page, ui);
    const tray = page.getByRole("complementary", { name: "未归类设备" });
    const dialog = page.locator("dialog.topology-dialog");
    const detail = page.getByRole("complementary", { name: "拓扑详情" });
    const trayDropPoint = async () => {
      // 详情面板开合会改变托盘位置，每次拖动前重新读取。
      const box = await tray.boundingBox();
      expect(box).not.toBeNull();
      return { x: box!.x + box!.width / 2, y: box!.y + 40 };
    };

    const deviceBox = await canvasNodeBox(page, "device", "工作站");
    await dragByMouse(page, deviceBox, await trayDropPoint());
    await expect(dialog).toBeVisible();
    await expect(dialog).toContainText("工作站");
    await expect(dialog).toContainText("家A局域网 · 192.168.1.20");
    await dialog.getByRole("button", { name: "取消" }).click();
    await expect(dialog).not.toBeVisible();
    await expect(tray).toHaveAttribute("data-count", "2");
    await selectTopologyEntity(page, "topology:select-device", "工作站");
    await expectMembershipRow(detail, "家A局域网", "192.168.1.20");
    await page.waitForTimeout(400);

    const deviceAgain = await canvasNodeBox(page, "device", "工作站");
    await dragByMouse(page, deviceAgain, await trayDropPoint());
    await expect(dialog).toBeVisible();
    await dialog.getByRole("button", { name: "确认移回" }).click();
    await expect(dialog).not.toBeVisible();
    await expect(tray).toHaveAttribute("data-count", "3");
    await expect(tray.getByRole("button", { name: /工作站/ })).toBeVisible();
    await selectTopologyEntity(page, "topology:select-device", "工作站");
    await expectNoMembershipRow(detail, "家A局域网");
    await expectMembershipRow(detail, "虚拟网", "100.64.0.20");
  } finally {
    await ui.close();
  }
});

test("虚拟视图拖动加入与退出虚拟局域网", async ({ page, isMobile }) => {
  test.skip(isMobile, "拖动归类用例只在桌面视口运行");
  const ui = await startStandaloneUIWithTopology();
  try {
    await openTopologyPage(page, ui);
    await page.getByRole("button", { name: "虚拟视图" }).click();
    const dialog = page.locator("dialog.topology-dialog");
    const detail = page.getByRole("complementary", { name: "拓扑详情" });

    // 拖出虚拟局域网框等于退出，需先确认。
    const nasBox = await canvasNodeBox(page, "device", "NAS");
    const empty = await emptyCanvasPoint(page);
    await dragByMouse(page, nasBox, empty);
    await expect(dialog).toBeVisible();
    await expect(dialog).toContainText("NAS");
    await expect(dialog).toContainText("虚拟网");
    await dialog.getByRole("button", { name: "确认退出" }).click();
    await expect(dialog).not.toBeVisible();
    await selectTopologyEntity(page, "topology:select-device", "NAS");
    await expectNoMembershipRow(detail, "虚拟网");
    await expectMembershipRow(detail, "家A局域网", "192.168.1.30");
    await page.waitForTimeout(400);

    // 拖入另一个虚拟局域网框等于加入，填地址后生效。
    const nasAgain = await canvasNodeBox(page, "device", "NAS");
    const target = await canvasNodeBox(page, "network", "虚拟网B");
    await dragByMouse(page, nasAgain, target);
    await expect(dialog).toBeVisible();
    await dialog.locator('input[name="address"]').fill("100.65.0.30");
    await dialog.getByRole("button", { name: "加入网络" }).click();
    await expect(dialog).not.toBeVisible();
    await selectTopologyEntity(page, "topology:select-device", "NAS");
    await expectMembershipRow(detail, "虚拟网B", "100.65.0.30");
  } finally {
    await ui.close();
  }
});

test("设备详情面板编辑网络成员地址、加入与退出", async ({ page, isMobile }) => {
  test.skip(isMobile, "拖动归类用例只在桌面视口运行");
  const ui = await startStandaloneUIWithTopology();
  try {
    await openTopologyPage(page, ui);
    const detail = page.getByRole("complementary", { name: "拓扑详情" });
    await selectTopologyEntity(page, "topology:select-device", "工作站");
    await expectMembershipRow(detail, "家A局域网", "192.168.1.20");

    // 改地址。
    const homeRow = detail.locator(".detail-member-form", { hasText: "家A局域网" });
    await homeRow.locator('input[name="address"]').fill("192.168.1.25");
    await homeRow.getByRole("button", { name: "保存地址" }).click();
    await expectMembershipRow(detail, "家A局域网", "192.168.1.25");
    await page.waitForTimeout(400);

    // 增加网络成员。
    const addForm = detail.locator(".detail-member-add");
    await addForm.locator("select[name=\"network\"]").selectOption({ label: "机房局域网（物理局域网）" });
    await addForm.locator('input[name="address"]').fill("10.0.0.28");
    await addForm.getByRole("button", { name: "加入网络" }).click();
    await expectMembershipRow(detail, "机房局域网", "10.0.0.28");
    await page.waitForTimeout(400);

    // 退出网络成员。
    const virtualB = detail.locator(".detail-member-form", { hasText: "虚拟网B" });
    await virtualB.getByRole("button", { name: "退出" }).click();
    const dialog = page.locator("dialog.topology-dialog");
    await expect(dialog).toBeVisible();
    await expect(dialog).toContainText("虚拟网B");
    await dialog.getByRole("button", { name: "确认退出" }).click();
    await expect(dialog).not.toBeVisible();
    await expectNoMembershipRow(detail, "虚拟网B");
    await expectMembershipRow(detail, "虚拟网", "100.64.0.20");
  } finally {
    await ui.close();
  }
});
