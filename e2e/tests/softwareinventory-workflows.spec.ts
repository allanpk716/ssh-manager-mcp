import { expect, gotoUIPath, test } from "../helpers";
import { readFile } from "node:fs/promises";

function parseDownloadedCSV(blob: string): string[][] {
  if (!blob.startsWith("\uFEFF")) {
    throw new Error(`下载的 CSV 缺少 UTF-8 字节顺序标记：${JSON.stringify(blob.slice(0, 20))}`);
  }
  return blob
    .slice(1)
    .trimEnd()
    .split("\r\n")
    .map((line) => line.split(","));
}

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

test("导出当前筛选下载 CSV 且页面保留筛选状态", async ({ page, softwareInventoryUI }) => {
  await openSoftwareInventory(page, softwareInventoryUI);
  await page.getByLabel("地点").selectOption({ label: "公司" });
  await page.getByLabel("网络").selectOption({ label: "总部物理局域网（物理局域网）" });
  await page.getByLabel("软件名（子串）").fill("openssl");
  await page.getByLabel("精确版本（可选）").fill("3.0.2-0ubuntu1.18");
  await page.locator("#software-inventory-search-form").getByRole("button", { name: "搜索" }).click();
  const searchResults = page.locator("h2:has-text('跨设备搜索结果') + ul.record-cards");
  await expect(searchResults).toContainText("工作站");

  const [download] = await Promise.all([
    page.waitForEvent("download"),
    page.getByRole("link", { name: "导出当前筛选" }).click(),
  ]);
  expect(download.suggestedFilename()).toBe("software-inventory.csv");
  const downloadPath = await download.path();
  const rows = parseDownloadedCSV(await readFile(downloadPath!, "utf8"));
  expect(rows).toHaveLength(3);
  expect(rows[0]).toEqual(["设备", "软件名", "版本", "类别", "监听端口", "暴露级别", "采集时间"]);
  expect(rows.slice(1).map((row) => row[1]).sort()).toEqual(["openssl", "openssl-library"]);
  for (const row of rows.slice(1)) {
    expect(row[0]).toBe("工作站");
  }

  await expect(page).toHaveURL(/site=/);
  await expect(page).toHaveURL(/network=/);
  await expect(page.getByLabel("软件名（子串）")).toHaveValue("openssl");
  await expect(page.getByLabel("精确版本（可选）")).toHaveValue("3.0.2-0ubuntu1.18");
});

test("设备详情导出这台设备的 CSV", async ({ page, softwareInventoryUI }) => {
  await openSoftwareInventory(page, softwareInventoryUI);
  await gotoUIPath(page, "/ui/software-inventory/devices?device=工作站");
  await page.getByRole("link", { name: "导出这台设备" }).waitFor();

  const [download] = await Promise.all([
    page.waitForEvent("download"),
    page.getByRole("link", { name: "导出这台设备" }).click(),
  ]);
  expect(download.suggestedFilename()).toBe("software-inventory-device.csv");
  const downloadPath = await download.path();
  const rows = parseDownloadedCSV(await readFile(downloadPath!, "utf8"));
  expect(rows).toHaveLength(11);
  expect(rows[0]).toEqual(["设备", "软件名", "版本", "类别", "监听端口", "暴露级别", "采集时间"]);
  for (const row of rows.slice(1)) {
    expect(row[0]).toBe("工作站");
  }
  const software = rows.slice(1).map((row) => `${row[1]} ${row[2]}`);
  expect(software).toContain("nginx 1.24.0-2ubuntu7");
  expect(software).toContain("openssl 3.0.2-0ubuntu1.18");
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
  await expect(page).toHaveURL(/\/ui\/software-inventory\/round$/);
  const round = page.locator("section.software-inventory-round");
  for (const device of ["工作站", "虚拟网设备", "分部过期设备", "分部失败设备", "old-1"]) {
    await expect(round.getByRole("heading", { name: device, exact: true })).toBeVisible();
  }
  await expect(round.getByText(/已登记，等待客户端实例 laptop-client 执行/)).toHaveCount(5);
  await expect(round).toContainText("等待客户端 5 台");
  await expect(round).not.toContainText("不受管存储");
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
