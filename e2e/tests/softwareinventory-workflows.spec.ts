import { expect, openStandaloneUI, openAuthorityUI, test } from "../helpers";

test("从待办认领未识别进程后它从待办消失", async ({ page, standaloneUI }) => {
  await openStandaloneUI(page, standaloneUI);

  await page.getByRole("link", { name: "软件清单" }).click();
  await page.getByRole("link", { name: "待办" }).click();
  await expect(page.getByRole("heading", { name: "未识别进程" })).toBeVisible();
  const processRow = page.locator("tr", { hasText: "/opt/gateway/gateway" });
  await expect(processRow).toContainText("0.0.0.0:8443/tcp");
  await processRow.getByRole("link", { name: "认领" }).click();

  await expect(page.getByRole("heading", { name: "新增软件声明" })).toBeVisible();
  await expect(page.getByLabel("设备")).toHaveValue("工作站");
  await expect(page.getByLabel(/监听端口/)).toHaveValue(
    "0.0.0.0,8443,tcp,all_addresses",
  );
  await page.getByLabel("软件名").fill("业务网关");
  await page.getByLabel("精确版本").fill("2.1.0");
  await page.getByLabel(/监听端口/).fill("0.0.0.0,8443,tcp,virtual_local_area_network");
  await page.getByRole("button", { name: "保存声明" }).click();

  await expect(page.getByRole("heading", { name: "软件清单待办" })).toBeVisible();
  await expect(page.getByText("已登记软件声明。")).toBeVisible();
  await expect(page.locator("tr", { hasText: "/opt/gateway/gateway" })).toHaveCount(0);
});

test("声明草稿确认后进入生效清单", async ({ page, standaloneUI }) => {
  await openStandaloneUI(page, standaloneUI);

  await page.getByRole("link", { name: "软件清单" }).click();
  await page.getByRole("link", { name: "草稿审阅" }).click();
  await expect(page.getByRole("heading", { name: "声明草稿审阅" })).toBeVisible();
  await expect(page.getByText("业务网关")).toBeVisible();
  await page.getByRole("button", { name: "按草稿确认" }).click();

  await expect(page.getByText("草稿已确认并进入生效清单。")).toBeVisible();
  await page.getByRole("link", { name: "软件清单" }).click();
  await page.getByRole("link", { name: "工作站" }).click();
  await expect(page.getByRole("heading", { name: "工作站 · 软件清单" })).toBeVisible();
  await expect(page.getByRole("cell", { name: "业务网关" }).first()).toBeVisible();
});

test("声明草稿驳回后消失", async ({ page, standaloneUI }) => {
  await openStandaloneUI(page, standaloneUI);

  await page.getByRole("link", { name: "软件清单" }).click();
  await page.getByRole("link", { name: "草稿审阅" }).click();
  await expect(page.getByText("业务网关")).toBeVisible();
  await page.getByRole("button", { name: "驳回草稿" }).click();

  await expect(page.getByText("草稿已驳回。")).toBeVisible();
  await expect(page.getByText("业务网关")).toHaveCount(0);
});

test("打开 sudo 采集开关要求重新验证并保留表单状态", async ({ page, authorityUI }) => {
  test.skip(test.info().project.name !== "desktop-chromium", "只在桌面浏览器等待重新验证时效");
  test.setTimeout(30_000);
  await openAuthorityUI(page, authorityUI, "browser-test-password");

  await page.getByRole("link", { name: "软件清单" }).click();
  await page.getByRole("link", { name: "采集设置" }).click();
  await page.getByLabel("服务器条目", { exact: true }).selectOption("web-1");
  await page.getByLabel("允许用 sudo 采集").check();

  // 端到端测试把重新验证窗口缩短到 100 毫秒；等它过期后再提交。
  await page.waitForTimeout(1_000);
  await page.getByRole("button", { name: "保存 sudo 采集开关" }).click();
  await expect(page.getByRole("heading", { name: "重新验证" })).toBeVisible();
  await page.getByLabel("管理员口令").fill("browser-test-password");
  await page.getByRole("button", { name: "验证并继续" }).click();

  await expect(page.getByRole("heading", { name: "软件清单采集设置" })).toBeVisible();
  await expect(page.getByText("已保留表单状态，请再次提交。")).toBeVisible();
  await expect(page.getByLabel("服务器条目", { exact: true })).toHaveValue("web-1");
  await expect(page.getByLabel("允许用 sudo 采集")).toBeChecked();
  await page.getByRole("button", { name: "保存 sudo 采集开关" }).click();
  await expect(page.getByText("sudo 采集开关已打开。")).toBeVisible();
});
