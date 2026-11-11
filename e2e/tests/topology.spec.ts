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
