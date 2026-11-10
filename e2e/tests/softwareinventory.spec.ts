import { expect, openClientUI, openStandaloneUI, test } from "../helpers";

test("设备清单页系统包默认折叠并可展开", async ({ page, standaloneUI }) => {
  await openStandaloneUI(page, standaloneUI);

  await page.getByRole("link", { name: "软件清单" }).click();
  await page.getByRole("link", { name: "工作站" }).click();

  await expect(page.getByRole("heading", { name: "工作站 · 软件清单" })).toBeVisible();
  await expect(page.getByText("系统包（2 条，默认折叠）")).toBeVisible();
  const firstPackage = page.locator("details tr", { hasText: "base-files" });
  await expect(firstPackage).toBeHidden();

  await page.getByText("展开系统包").click();
  await expect(firstPackage).toBeVisible();
  await expect(page.locator("details tr", { hasText: "zlib1g" })).toBeVisible();
});

test("客户端 Web UI 没有软件清单入口", async ({ page, clientUI }) => {
  await openClientUI(page, clientUI);

  await expect(page.getByRole("heading", { name: "服务器" })).toBeVisible();
  await expect(page.getByRole("navigation", { name: "软件清单区域" })).toHaveCount(0);
  await expect(page.getByRole("link", { name: "软件清单" })).toHaveCount(0);
});
