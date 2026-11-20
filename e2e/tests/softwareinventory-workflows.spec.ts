import { expect, gotoUIPath, test } from "../helpers";

async function openSoftwareInventory(
  page: import("@playwright/test").Page,
  ui: { startupURL: string },
): Promise<void> {
  await page.goto(ui.startupURL);
  await page.getByRole("heading", { name: "状态总览" }).waitFor();
  await gotoUIPath(page, "/ui/software-inventory/");
  await page.getByRole("heading", { name: "软件清单", exact: true }).waitFor();
}

test("地点和网络筛选即选即生效并保留地址与表单状态", async ({ page, softwareInventoryUI }) => {
  await openSoftwareInventory(page, softwareInventoryUI);
  const deviceList = page.locator("h2:has-text('设备清单') + ul.record-cards");
  await expect(deviceList).toContainText("工作站");
  await expect(deviceList).toContainText("虚拟网设备");
  await expect(deviceList).toContainText("分部过期设备");

  await page.getByLabel("地点").selectOption({ label: "公司" });
  await expect(deviceList).toContainText("工作站");
  await expect(deviceList).toContainText("虚拟网设备");
  await expect(deviceList).not.toContainText("分部过期设备");

  await page.getByLabel("网络").selectOption({ label: "总部物理局域网（物理局域网）" });
  await expect(deviceList).toContainText("工作站");
  await expect(deviceList).not.toContainText("虚拟网设备");
  await expect(page).toHaveURL(/site=/);
  await expect(page).toHaveURL(/network=/);

  await page.getByLabel("软件名（子串）").fill("openssl");
  await page.getByLabel("精确版本（可选）").fill("3.0.2-0ubuntu1.18");
  await page.locator("#software-inventory-search-form").getByRole("button", { name: "搜索" }).click();
  const searchResults = page.locator("h2:has-text('跨设备搜索结果') + ul.record-cards");
  await expect(searchResults).toContainText("工作站");
  await expect(searchResults).not.toContainText("分部过期设备");

  await page.reload();
  await expect(page.getByLabel("地点")).toHaveValue("公司");
  await expect(page.getByLabel("网络")).toHaveValue("总部物理局域网");
  await expect(page.getByLabel("软件名（子串）")).toHaveValue("openssl");
  await expect(page.getByLabel("精确版本（可选）")).toHaveValue("3.0.2-0ubuntu1.18");
  await expect(searchResults).toContainText("工作站");
});

test("全局搜索支持精确版本并默认折叠全部软件包", async ({ page, softwareInventoryUI }) => {
  await openSoftwareInventory(page, softwareInventoryUI);
  const searchResults = page.locator("h2:has-text('跨设备搜索结果') + ul.record-cards");
  await page.getByLabel("软件名（子串）").fill("openssl");
  await page.getByLabel("精确版本（可选）").fill("3.0.2-0ubuntu1.18");
  await page.locator("#software-inventory-search-form").getByRole("button", { name: "搜索" }).click();

  await expect(searchResults.locator("li.record-card")).toHaveCount(2);
  await expect(searchResults).toContainText("工作站");
  await expect(searchResults).toContainText("分部过期设备");
  await expect(searchResults).not.toContainText("虚拟网设备");
  await expect(searchResults).toContainText("3.0.2-0ubuntu1.18");

  await page.getByLabel("地点").selectOption({ label: "公司" });
  await expect(searchResults).toContainText("工作站");
  await expect(searchResults).not.toContainText("分部过期设备");

  await page.getByLabel("地点").selectOption({ label: "全部地点" });
  await page.getByLabel("精确版本（可选）").fill("");
  await page.locator("#software-inventory-search-form").getByRole("button", { name: "搜索" }).click();
  await expect(searchResults.locator("li.record-card")).toHaveCount(3);
  await expect(searchResults).toContainText("虚拟网设备");

  const virtualCard = searchResults.locator("li.record-card").filter({ hasText: "虚拟网设备" });
  await expect(virtualCard).toContainText("OpenSSL");
  await expect(virtualCard.getByText("openssl-library")).toBeHidden();
  await virtualCard.getByText("展开全部软件包").click();
  await expect(virtualCard.getByText("openssl-library")).toBeVisible();
});

test("每天采集时间持久保存且立即全部采集登记客户端设备", async ({ page, softwareInventoryScheduleUI }) => {
  await openSoftwareInventory(page, softwareInventoryScheduleUI);
  const dailyTime = page.getByLabel("每天采集时间");
  await expect(dailyTime).toHaveValue("01:00");
  await dailyTime.fill("02:30");
  await page.getByRole("button", { name: "保存时间" }).click();
  await expect(page.getByText("每天采集时间已保存。")).toBeVisible();
  await page.reload();
  await expect(page.getByLabel("每天采集时间")).toHaveValue("02:30");

  await page.getByRole("button", { name: "立即全部采集" }).click();
  const report = page.locator("section[aria-label='立即全部采集报告']");
  await expect(report).toContainText("成功 0 台，等待客户端实例 5 台，失败 0 台");
  for (const device of ["工作站", "虚拟网设备", "分部过期设备", "分部失败设备", "old-1"]) {
    await expect(report).toContainText(`${device}：已登记，等待客户端实例 laptop-client 执行`);
  }
  await expect(report).not.toContainText("不受管存储");
});

test("软件清单区域撤下旧流程入口", async ({ page, softwareInventoryUI }) => {
  await openSoftwareInventory(page, softwareInventoryUI);
  const area = page.locator("section.software-inventory");
  for (const forbidden of ["声明", "认领", "草稿", "待办", "对不上", "待确认暴露", "采集设置"]) {
    await expect(area).not.toContainText(forbidden);
  }
  await expect(area.getByRole("link", { name: /声明|草稿|待办/ })).toHaveCount(0);

  for (const path of [
    "/ui/software-inventory/todos",
    "/ui/software-inventory/declarations",
    "/ui/software-inventory/declarations/new",
    "/ui/software-inventory/drafts",
    "/ui/software-inventory/settings",
  ]) {
    await gotoUIPath(page, path);
    await expect(page.getByRole("heading", { name: "页面不存在" })).toBeVisible();
  }
});
