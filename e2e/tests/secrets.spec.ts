import { expect, openStandaloneUI, test } from "../helpers";

test("新增机密项后列表不显示值", async ({ page, standaloneUI }) => {
  await openStandaloneUI(page, standaloneUI);

  await page.getByRole("link", { name: "机密项" }).click();
  await page.getByRole("link", { name: "新增机密项" }).click();
  await page.getByLabel("名字").fill("BROWSER_TOKEN");
  await page.getByLabel("备注").fill("浏览器新增");
  await page.getByLabel("环境变量名（可选）").fill("BROWSER_TOKEN_FILE");
  await page.getByLabel("值", { exact: true }).fill("browser-entered-secret-value");
  await page.getByRole("button", { name: "新增" }).click();

  await expect(page.getByRole("heading", { name: "机密项" })).toBeVisible();
  await expect(page.getByRole("link", { name: "BROWSER_TOKEN" })).toBeVisible();
  await expect(page.getByText("浏览器新增")).toBeVisible();
  await expect(page.getByText("BROWSER_TOKEN_FILE")).toBeVisible();
  expect(await page.content()).not.toContain("browser-entered-secret-value");
});

test("机密项矩阵整列勾选后预览确认并显示结果", async ({ page, standaloneUI }) => {
  await openStandaloneUI(page, standaloneUI);

  await page.getByRole("link", { name: "机密项" }).click();
  await page.getByRole("link", { name: "授权矩阵" }).click();
  const webColumn = page.locator("th", { hasText: "web-1" }).first();
  await webColumn.getByRole("button", { name: "整列勾选" }).click();
  await page.getByRole("button", { name: "预览授权变更" }).click();

  await expect(page.getByRole("heading", { name: "机密项授权预览" })).toBeVisible();
  await expect(page.getByText("新增 1 个授权，移除 0 个授权。")).toBeVisible();
  await page.getByRole("button", { name: "确认保存授权" }).click();

  await expect(page.getByText("已保存授权变更。")).toBeVisible();
  expect(await page.content()).not.toContain("browser-test-secret-value");
});
