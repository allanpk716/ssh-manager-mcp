import { expect, test } from "../helpers";
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
    await expect(detail).toContainText("家A局域网（物理局域网）· 192.168.1.20");
    await expect(detail).toContainText("共同物理局域网");
    await expect(detail.getByRole("button", { name: "关闭" })).toBeVisible();

    await selectTopologyEntity(page, "topology:select-network", "家A局域网");
    await expect(detail).toContainText("192.168.1.0/24");
    await expect(detail).toContainText("1000 Mbit/s");
    await expect(detail).toContainText("按流量计费是");

    await selectTopologyEntity(page, "topology:select-site", "家A");
    await expect(detail).toContainText("家A局域网 · 2 台成员设备");
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
  } finally {
    await ui.close();
  }
});

type TopologyIdentifiers = {
  devices: Record<string, string>;
  sites: Record<string, string>;
  networks: Record<string, string>;
};

async function topologyIdentifiers(
  page: import("@playwright/test").Page,
): Promise<TopologyIdentifiers> {
  return page.evaluate(async () => {
    const response = await fetch("/ui/api/topology", {
      headers: { Accept: "application/json" },
      credentials: "same-origin",
    });
    if (!response.ok) {
      throw new Error(`读取拓扑数据失败（${response.status}）。`);
    }
    const data = await response.json();
    const devices: Record<string, string> = {};
    const sites: Record<string, string> = {};
    const networks: Record<string, string> = {};
    data.devices.forEach((device: { name: string; id: string }) => {
      devices[device.name] = device.id;
    });
    data.sites.forEach((site: { name: string; id: string }) => {
      sites[site.name] = site.id;
    });
    data.networks.forEach((network: { name: string; id: string }) => {
      networks[network.name] = network.id;
    });
    return { devices, sites, networks };
  });
}

async function moveTopologyEntity(
  page: import("@playwright/test").Page,
  entityType: string,
  entityID: string,
  x: number,
  y: number,
): Promise<void> {
  await page.evaluate(
    ({ type, id, targetX, targetY }) => {
      document
        .querySelector("[data-topology-page]")
        ?.dispatchEvent(
          new CustomEvent("topology:move", {
            detail: { entity_type: type, entity_id: id, x: targetX, y: targetY },
            bubbles: true,
          }),
        );
    },
    { type: entityType, id: entityID, targetX: x, targetY: y },
  );
}

async function waitForCanvasSettled(
  page: import("@playwright/test").Page,
): Promise<void> {
  await page.waitForTimeout(500);
}

async function currentCanvasLayout(
  page: import("@playwright/test").Page,
): Promise<{
  view: string;
  devices: { device_id: string; x: number; y: number }[];
  containers: { entity_type: string; entity_id: string; x: number; y: number }[];
}> {
  const raw = await page.locator("#topology-canvas").getAttribute("data-layout-json");
  expect(raw).not.toBeNull();
  return JSON.parse(raw!);
}

test("桌面端布局按视图保存、刷新后保持并可重置", async ({ page, isMobile }) => {
  test.skip(isMobile, "桌面交互用例只在桌面视口运行");
  const ui = await startStandaloneUIWithTopology();
  try {
    await openTopologyPage(page, ui);
    const canvas = page.locator("#topology-canvas");
    const status = page.locator("[data-topology-status]");
    const ids = await topologyIdentifiers(page);
    await waitForCanvasSettled(page);

    await moveTopologyEntity(page, "device", ids.devices["工作站"], 333, 444);
    await expect(status).toHaveText("布局已保存。");
    let layout = await currentCanvasLayout(page);
    let moved = layout.devices.find((item) => item.device_id === ids.devices["工作站"]);
    expect(moved).toMatchObject({ x: 333, y: 444 });

    // 站点区域整体移动后同样保存；坐标记录的是区域框的中心，区域内设备
    // 的坐标也随这次整体移动更新。
    await moveTopologyEntity(page, "site", ids.sites["家A"], 2600, 1800);
    await expect(status).toHaveText("布局已保存。");
    layout = await currentCanvasLayout(page);
    const siteCoordinate = layout.containers.find(
      (item) => item.entity_type === "site" && item.entity_id === ids.sites["家A"],
    );
    expect(siteCoordinate).toBeDefined();
    expect(Math.abs(siteCoordinate!.x - 2600)).toBeLessThanOrEqual(2);
    expect(Math.abs(siteCoordinate!.y - 1800)).toBeLessThanOrEqual(2);
    moved = layout.devices.find((item) => item.device_id === ids.devices["工作站"]);
    expect(moved).toBeDefined();
    expect(moved!.x).toBeGreaterThan(2500);
    const physicalAfterSiteMove = { x: moved!.x, y: moved!.y };

    // 另一个视图独立保存；切回物理视图后仍是物理视图保存的坐标。
    await page.getByRole("button", { name: "虚拟视图" }).click();
    await expect(canvas).toHaveAttribute("data-view", "virtual");
    await waitForCanvasSettled(page);
    await moveTopologyEntity(page, "device", ids.devices["工作站"], 555, 666);
    await expect(status).toHaveText("布局已保存。");
    layout = await currentCanvasLayout(page);
    expect(layout.view).toBe("virtual");
    moved = layout.devices.find((item) => item.device_id === ids.devices["工作站"]);
    expect(moved).toMatchObject({ x: 555, y: 666 });

    await page.getByRole("button", { name: "物理视图" }).click();
    await expect(canvas).toHaveAttribute("data-view", "physical");
    await waitForCanvasSettled(page);
    layout = await currentCanvasLayout(page);
    moved = layout.devices.find((item) => item.device_id === ids.devices["工作站"]);
    expect(moved).toMatchObject(physicalAfterSiteMove);

    // 刷新页面后保存的布局仍然生效。
    await page.reload();
    await page.getByRole("heading", { name: "网络拓扑" }).waitFor();
    await expect(canvas).toHaveAttribute("data-layout-json", /.+/);
    await waitForCanvasSettled(page);
    layout = await currentCanvasLayout(page);
    moved = layout.devices.find((item) => item.device_id === ids.devices["工作站"]);
    expect(moved).toMatchObject(physicalAfterSiteMove);

    // 重置需要确认；确认后物理视图回到自动排布，虚拟视图坐标不受影响。
    page.once("dialog", (dialog) => {
      void dialog.accept();
    });
    await page.getByRole("button", { name: "重置布局" }).click();
    await expect(status).toHaveText("布局已重置。");
    await waitForCanvasSettled(page);
    layout = await currentCanvasLayout(page);
    moved = layout.devices.find((item) => item.device_id === ids.devices["工作站"]);
    expect(moved).toBeDefined();
    expect(moved!).not.toMatchObject(physicalAfterSiteMove);

    await page.reload();
    await page.getByRole("heading", { name: "网络拓扑" }).waitFor();
    await expect(canvas).toHaveAttribute("data-layout-json", /.+/);
    await waitForCanvasSettled(page);
    layout = await currentCanvasLayout(page);
    moved = layout.devices.find((item) => item.device_id === ids.devices["工作站"]);
    expect(moved!).not.toMatchObject(physicalAfterSiteMove);

    await page.getByRole("button", { name: "虚拟视图" }).click();
    await expect(canvas).toHaveAttribute("data-view", "virtual");
    await waitForCanvasSettled(page);
    layout = await currentCanvasLayout(page);
    moved = layout.devices.find((item) => item.device_id === ids.devices["工作站"]);
    expect(moved).toMatchObject({ x: 555, y: 666 });
  } finally {
    await ui.close();
  }
});

test("容器区域明显重叠时提示并还原本次移动", async ({ page, isMobile }) => {
  test.skip(isMobile, "桌面交互用例只在桌面视口运行");
  const ui = await startStandaloneUIWithTopology();
  try {
    await openTopologyPage(page, ui);
    const status = page.locator("[data-topology-status]");
    const ids = await topologyIdentifiers(page);

    await page.getByRole("button", { name: "虚拟视图" }).click();
    await expect(page.locator("#topology-canvas")).toHaveAttribute("data-view", "virtual");
    await waitForCanvasSettled(page);
    const layoutBefore = await currentCanvasLayout(page);
    const target = layoutBefore.containers.find(
      (item) => item.entity_type === "site" && item.entity_id === ids.sites["机房"],
    );
    expect(target).toBeDefined();

    await moveTopologyEntity(page, "site", ids.sites["家A"], target!.x, target!.y);
    await expect(status).toHaveText("区域与其他区域明显重叠，已还原本次移动。");
    await waitForCanvasSettled(page);
    const layoutAfter = await currentCanvasLayout(page);
    const restored = layoutAfter.containers.find(
      (item) => item.entity_type === "site" && item.entity_id === ids.sites["家A"],
    );
    const before = layoutBefore.containers.find(
      (item) => item.entity_type === "site" && item.entity_id === ids.sites["家A"],
    );
    expect(restored).toMatchObject({ x: before!.x, y: before!.y });
  } finally {
    await ui.close();
  }
});

test("客户端与手机没有布局保存入口", async ({ page, isMobile }) => {
  const ui = isMobile ? await startStandaloneUIWithTopology() : await startClientUIWithTopology();
  try {
    if (isMobile) {
      await openTopologyPage(page, ui);
    } else {
      await openClientUI(page, ui);
      await page.getByRole("link", { name: "网络拓扑" }).click();
      await page.getByRole("heading", { name: "网络拓扑" }).waitFor();
    }
    const canvas = page.locator("#topology-canvas");
    await expect(canvas).toHaveAttribute("data-grabbable", "false");
    await expect(page.getByRole("button", { name: "重置布局" })).toBeHidden();

    const saveStatus = await page.evaluate(async () => {
      const meta = document.querySelector('meta[name="csrf-token"]');
      const token = meta === null ? "" : meta.getAttribute("content") ?? "";
      const response = await fetch("/ui/api/topology/layout", {
        method: "POST",
        headers: { "Content-Type": "application/json", "X-CSRF-Token": token },
        credentials: "same-origin",
        body: JSON.stringify({ view: "physical", devices: [], containers: [] }),
      });
      return response.status;
    });
    if (!isMobile) {
      // 客户端角色不注册布局写路由；手机用例只验证界面入口不可用。
      expect(saveStatus).toBe(404);
    }
  } finally {
    await ui.close();
  }
});
