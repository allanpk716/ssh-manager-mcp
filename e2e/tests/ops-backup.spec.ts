import { expect, openStandaloneUI, test } from "../helpers";

test("备份页可先配置再立即备份并一键校验最新备份", async ({ page, standaloneUI }) => {
  await openStandaloneUI(page, standaloneUI);

  await page.getByRole("link", { name: "运维" }).click();
  await page.getByRole("link", { name: "备份" }).click();
  await expect(page.getByRole("heading", { name: "备份", exact: true })).toBeVisible();
  await expect(page.locator(".backup-alert", { hasText: "尚未配置备份" })).toBeVisible();

  await page.getByLabel("备份目录").fill(standaloneUI.backupDir);
  await page.getByLabel("保留份数").fill("5");
  await page.getByLabel("口令文件").fill(standaloneUI.passphraseFile);
  // 移动浏览器聚焦输入框后会放大视觉视口；在最后一个输入框按回车提交，
  // 与真人操作一致，也避免点击坐标被放大视口偏移。
  await page.getByLabel("口令文件").press("Enter");
  await expect(page.locator(".backup-flash", { hasText: "备份配置已保存" })).toBeVisible();
  await expect(page.locator(".backup-empty", { hasText: "尚无备份" })).toBeVisible();

  await page.getByRole("button", { name: "立即备份" }).click();
  const createdFlash = page.locator(".backup-flash", { hasText: "已备份" });
  await expect(createdFlash).toBeVisible();
  const createdFile = (await createdFlash.textContent())?.split("已备份：")[1]?.trim() ?? "";
  expect(createdFile).toMatch(/^vault-\d{8}-\d{6}\.sme$/);
  await expect(page.locator(".backup-verify-state", { hasText: "已校验" })).toBeVisible();

  await page.getByRole("button", { name: "校验最新备份" }).click();
  await expect(page.locator(".backup-flash", { hasText: "已校验" })).toBeVisible();
  await expect(page.locator(".backup-verify-state", { hasText: "已校验" })).toBeVisible();
  await expect(page.getByText(createdFile).first()).toBeVisible();
});
