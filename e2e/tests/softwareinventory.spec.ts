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

test("设备详情按类别分组并展示未识别进程信息", async ({ page, softwareInventoryUI }) => {
  await openSoftwareInventory(page, softwareInventoryUI);
  await gotoUIPath(page, "/ui/software-inventory/devices?device=工作站");
  await page.getByRole("heading", { name: "工作站 · 软件清单" }).waitFor();
  const area = page.locator("section.software-inventory");

  await expect(area).toContainText("Ubuntu 24.04.1 LTS；Linux 内核 6.8.0-49-generic");
  await expect(area).toContainText("重点软件");
  await expect(area).toContainText("全部软件");
  await expect(area).toContainText("未识别进程");
  await expect(area).toContainText("采集时间");
  await expect(area).toContainText("正常");

  for (const name of ["操作系统与内核", "服务程序", "容器镜像", "驱动与固件", "未识别进程"]) {
    await expect(area.getByRole("heading", { name, exact: true })).toBeVisible();
  }
  await expect(area.getByRole("heading", { name: /系统包（\d+ 条，默认折叠）/ })).toBeVisible();
  const packages = area.locator("h2", { hasText: "系统包" }).locator("xpath=following-sibling::details[1]");
  await expect(packages.getByRole("heading", { name: "openssl-library", exact: true })).toBeHidden();
  await packages.locator("summary").click();
  await expect(packages.getByRole("heading", { name: "openssl-library", exact: true })).toBeVisible();

  const unidentified = area.locator("li.record-card").filter({ hasText: "未识别" }).first();
  await expect(unidentified).toContainText("gateway");
  await expect(unidentified).toContainText("/opt/gateway/gateway");
  await expect(unidentified).toContainText("0.0.0.0:8443/tcp");
  await expect(unidentified).toContainText("监听所有地址");
  await expect(unidentified.getByRole("button")).toHaveCount(0);

  for (const forbidden of ["声明", "认领", "草稿", "待办", "对不上", "待确认暴露", "局域网", "公网"]) {
    await expect(area).not.toContainText(forbidden);
  }
});

test("设备详情高级采集设置默认折叠并支持采集", async ({ page, softwareInventoryUI }) => {
  await openSoftwareInventory(page, softwareInventoryUI);
  await gotoUIPath(page, "/ui/software-inventory/devices?device=工作站");
  await page.getByRole("heading", { name: "工作站 · 软件清单" }).waitFor();
  const advanced = page.locator("#software-inventory-advanced");
  await expect(advanced.getByLabel("采集执行者")).toBeHidden();
  await advanced.locator("summary").click();
  await expect(advanced.getByLabel("采集执行者")).toBeVisible();
  await expect(advanced.getByLabel("采集执行者")).toContainText("权威端");
  await expect(advanced.getByLabel("采集执行者")).toContainText("laptop-client");

  await advanced.getByLabel("采集执行者").selectOption("laptop-client");
  await advanced.getByLabel("采集用服务器条目").selectOption("build-1");
  await advanced.getByRole("button", { name: "保存采集执行者" }).click();
  await expect(page.getByText("不能把它登记为这台设备的采集执行者")).toBeVisible();

  // 校验错误后高级区保持展开，直接修正选择并再次提交。
  await advanced.getByLabel("采集执行者").selectOption("laptop-client");
  await advanced.getByLabel("采集用服务器条目").selectOption("web-1");
  await advanced.getByRole("button", { name: "保存采集执行者" }).click();
  await expect(page.getByText("采集执行者已登记。")).toBeVisible();

  await advanced.locator("summary").click();
  await advanced.getByLabel("服务器条目", { exact: true }).selectOption("web-1");
  await advanced.getByLabel("允许用 sudo 采集").check();
  await advanced.getByRole("button", { name: "保存 sudo 采集开关" }).click();
  await expect(page.getByText("sudo 采集开关已打开。")).toBeVisible();
  await advanced.locator("summary").click();
  await expect(advanced.locator("li.record-card").filter({ hasText: "web-1" })).toContainText("已打开");

  await page.getByRole("button", { name: "立即采集" }).click();
  await expect(page.getByText("已请求，等待客户端执行。")).toBeVisible();
  await expect(page.locator(".status-badges")).toContainText("等待客户端执行");
});
