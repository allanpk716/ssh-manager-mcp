import { expect, openStandaloneUI, test } from "../helpers";

test("审计页按来源过滤后可以继续翻页", async ({ page, standaloneUI }) => {
  await openStandaloneUI(page, standaloneUI);

  await page.getByRole("link", { name: "运维" }).click();
  await page.getByRole("link", { name: "审计" }).click();
  await page.getByLabel("来源", { exact: true }).selectOption("cli");
  await page.getByRole("button", { name: "查询" }).click();

  await expect(page.locator("tr.audit-row")).toHaveCount(100);
  await page.getByRole("link", { name: "更早的记录" }).click();

  await expect(page.locator("tr.audit-row")).toHaveCount(20);
  await expect(page).toHaveURL(/before=\d+/);
  await expect(page.getByLabel("来源", { exact: true })).toHaveValue("cli");
});
