import { expect, openStandaloneUI, test } from "../helpers";

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
