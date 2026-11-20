import { expect, gotoUIPath, openClientUI, test } from "../helpers";

async function openSoftwareInventory(
  page: import("@playwright/test").Page,
  ui: { startupURL: string },
): Promise<void> {
  await page.goto(ui.startupURL);
  await page.getByRole("heading", { name: "状态总览" }).waitFor();
  await gotoUIPath(page, "/ui/software-inventory/");
  await page.getByRole("heading", { name: "软件清单", exact: true }).waitFor();
}

test("软件清单首页按设备展示实测摘要与采集状态", async ({ page, softwareInventoryUI }) => {
  await openSoftwareInventory(page, softwareInventoryUI);

  const deviceCard = (name: string) =>
    page.locator(".record-card").filter({
      has: page.getByRole("heading", { name, exact: true }),
    });
  const workstation = deviceCard("工作站");
  await expect(workstation).toContainText("Ubuntu 24.04.1 LTS");
  await expect(workstation).toContainText("Linux 内核 6.8.0-49-generic");
  await expect(workstation).toContainText("重点软件");
  await expect(workstation).toContainText("6");
  await expect(workstation).toContainText("全部软件");
  await expect(workstation).toContainText("10");
  await expect(workstation).toContainText("未识别进程");
  await expect(workstation).toContainText("1");
  await expect(workstation).toContainText("采集时间");
  await expect(workstation).toContainText("正常");
  await expect(workstation.getByRole("button", { name: "立即采集" })).toBeVisible();

  for (const name of ["工作站", "虚拟网设备", "分部过期设备", "分部失败设备"]) {
    await expect(deviceCard(name)).toContainText("Ubuntu 24.04.1 LTS");
    await expect(deviceCard(name)).toContainText("Linux 内核 6.8.0-49-generic");
  }
  await expect(deviceCard("分部过期设备")).toContainText("清单过期");
  await expect(deviceCard("分部失败设备")).toContainText("采集失败");
  await expect(deviceCard("分部失败设备")).toContainText("连接超时，端口不通");
  await expect(deviceCard("不受管存储")).toContainText("不采集（不受管设备）");
  await expect(deviceCard("不受管存储").getByRole("button", { name: "立即采集" })).toHaveCount(0);
});

test("客户端 Web UI 没有软件清单入口", async ({ page, clientUI }) => {
  await openClientUI(page, clientUI);
  await expect(page.getByRole("link", { name: "软件清单" })).toHaveCount(0);

  await page.getByRole("navigation", { name: "一级导航" }).getByRole("link", { name: "更多" }).click();
  await page.getByRole("heading", { name: "更多" }).waitFor();
  await expect(page.getByRole("link", { name: "软件清单" })).toHaveCount(0);

  await gotoUIPath(page, "/ui/software-inventory/");
  await expect(page.getByRole("heading", { name: "页面不存在" })).toBeVisible();
});
