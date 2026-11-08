import { expect, openStandaloneUI, test } from "../helpers";

test("设置页切换配对开关后刷新仍保持", async ({ page, standaloneUI }) => {
  await openStandaloneUI(page, standaloneUI);

  await page.getByRole("link", { name: "设置", exact: true }).click();
  const pairing = page.locator(".setting-item", {
    has: page.getByRole("heading", { name: "serve.pairing", exact: true }),
  });
  await expect(pairing.getByLabel("设置存储值")).toHaveValue("true");
  await pairing.getByLabel("设置存储值").selectOption("false");
  await pairing.getByRole("button", { name: "保存" }).click();

  await expect(page).toHaveURL(/\/ui\/ops\/settings$/);
  await expect(pairing.getByText("当前值", { exact: true })).toBeVisible();
  await page.reload();

  await expect(pairing.getByLabel("设置存储值")).toHaveValue("false");
  await expect(pairing.getByText("false", { exact: true }).first()).toBeVisible();
});
