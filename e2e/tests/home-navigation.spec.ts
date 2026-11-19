import { mkdir } from "node:fs/promises";
import path from "node:path";
import { expect, openAuthorityUI, startAuthorityUI, test } from "../helpers";

const screenshotDirectory =
  "/Users/allan716/.cache/implement-spec-split/runs/ssh-manager-mcp-294-20261119-0809/impl-297/screenshots";

test("权威端主页展示五标签导航并可在主页与服务器页之间切换", async ({ page }, testInfo) => {
  const ui = await startAuthorityUI("browser-test-password");
  const mobile = testInfo.project.name === "mobile-chromium";
  await page.setViewportSize({ width: mobile ? 390 : 1280, height: mobile ? 844 : 800 });
  try {
    await openAuthorityUI(page, ui, ui.password);
    await expect(page.getByRole("heading", { name: "状态总览" })).toBeVisible();

    const navigation = page.getByRole("navigation", { name: "一级导航" });
    await expect(navigation.getByRole("link", { name: "主页" })).toBeVisible();
    await expect(navigation.getByRole("link", { name: "服务器" })).toBeVisible();
    await expect(navigation.getByRole("link", { name: "客户端" })).toBeVisible();
    await expect(navigation.getByRole("link", { name: "授权" })).toBeVisible();
    await expect(navigation.getByRole("link", { name: "更多" })).toBeVisible();

    const navigationBox = await navigation.boundingBox();
    expect(navigationBox).not.toBeNull();
    if (mobile) {
      expect(navigationBox!.y + navigationBox!.height).toBeGreaterThan(820);
    } else {
      expect(navigationBox!.y).toBeLessThan(100);
    }

    await mkdir(screenshotDirectory, { recursive: true });
    await page.screenshot({
      path: path.join(screenshotDirectory, mobile ? "home-390x844.png" : "home-1280x800.png"),
      fullPage: true,
    });

    await navigation.getByRole("link", { name: "服务器" }).click();
    await expect(page.getByRole("heading", { name: "服务器" })).toBeVisible();
    await expect(page).toHaveURL(/\/ui\/servers\/$/);
    await navigation.getByRole("link", { name: "主页" }).click();
    await expect(page.getByRole("heading", { name: "状态总览" })).toBeVisible();
  } finally {
    await ui.close();
  }
});
