import {
  expect,
  openStandaloneUI,
  startStandaloneUIWithTopology,
  test,
} from "../helpers";

test("共享存储可挂载范围勾选后预览并确认", async ({ page, sharesStandaloneUI }) => {
  await openStandaloneUI(page, sharesStandaloneUI);

  await page.getByRole("link", { name: "共享存储" }).click();
  await page.getByRole("link", { name: "datasets" }).click();
  const webCheckbox = page.getByRole("checkbox", { name: /web-1/ });
  const shareDBCheckbox = page.getByRole("checkbox", { name: /share-db/ });
  await webCheckbox.uncheck();
  await shareDBCheckbox.check();

  await expect(
    page.locator("tr", { hasText: "share-db" }).getByText("同设备的其他条目可能也能读到挂载内容。"),
  ).toBeVisible();
  await page.getByRole("button", { name: "预览可挂载范围变更" }).click();

  await expect(page.getByRole("heading", { name: "机密项授权预览" })).toBeVisible();
  await expect(page.getByText("新增 1 个授权，移除 1 个授权。")).toBeVisible();
  await page.getByRole("button", { name: "确认保存授权" }).click();

  await expect(page.getByText("已保存授权变更。")).toBeVisible();
  expect(await page.content()).not.toContain("share-mount-password-value");
});

test("共享存储桌面手动添加引导展示步骤且没有密码入口", async ({ page }) => {
  const ui = await startStandaloneUIWithTopology();
  try {
    await openStandaloneUI(page, ui);

    await page.getByRole("link", { name: "共享存储" }).click();
    await page.getByRole("link", { name: "家NAS共享" }).click();
    await page.getByRole("link", { name: "打开桌面手动添加引导" }).click();
    await page.getByLabel("这台电脑是拓扑里哪台设备").selectOption({ label: "工作站" });
    await page.getByRole("button", { name: "显示引导" }).click();

    await expect(page.getByRole("heading", { name: "桌面手动添加 家NAS共享" })).toBeVisible();
    await expect(page.getByText("192.168.1.30").first()).toBeVisible();
    await expect(page.getByText("家A局域网").first()).toBeVisible();
    await expect(page.getByText("N:").first()).toBeVisible();
    await expect(page.getByText("\\\\192.168.1.30\\家NAS共享\\photos").first()).toBeVisible();
    await expect(
      page.getByText("net use N: \\\\192.168.1.30\\家NAS共享 /user:backup * /persistent:yes"),
    ).toBeVisible();
    await expect(page.getByText("smb://192.168.1.30/家NAS共享").first()).toBeVisible();
    await expect(page.getByText("/Volumes/家NAS共享").first()).toBeVisible();
    await expect(page.getByText("笔记本更换网络后，映射可能失效")).toBeVisible();
    await expect(page.getByText("请输入你自己保存在密码管理器里的共享密码")).toBeVisible();
    expect(await page.content()).not.toContain("nas-login-secret");
    await expect(
      page.locator(
        'input[type="password"], input[name*="password" i], a:has-text("密码"), button:has-text("密码")',
      ),
    ).toHaveCount(0);
  } finally {
    await ui.close();
  }
});
