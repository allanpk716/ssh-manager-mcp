import { mkdir } from "node:fs/promises";
import path from "node:path";
import { expect, gotoUIPath, startStandaloneUIWithTopology, test } from "../helpers";

const screenshotDirectory =
  "/Users/allan716/.cache/implement-spec-split/runs/ssh-manager-mcp-294-20261119-0809/impl-299/screenshots";

async function openDetail(
  page: import("@playwright/test").Page,
  ui: { startupURL: string },
): Promise<void> {
  await page.setViewportSize({ width: 390, height: 844 });
  await page.goto(ui.startupURL);
  await page.getByRole("heading", { name: "状态总览" }).waitFor();
  await gotoUIPath(page, "/ui/servers/web-1");
  await page.getByRole("heading", { name: "web-1", exact: true }).waitFor();
}

test("服务器详情四块可切换并走通地点、地址与机密项授权", async ({ page }) => {
  const ui = await startStandaloneUIWithTopology();
  try {
    await openDetail(page, ui);
    const tabs = page.getByRole("navigation", { name: "服务器详情分段" });
    await expect(tabs.getByRole("link", { name: "连接" })).toBeVisible();
    await expect(tabs.getByRole("link", { name: "地址与地点" })).toBeVisible();
    await expect(tabs.getByRole("link", { name: "授权" })).toBeVisible();
    await expect(tabs.getByRole("link", { name: "软件" })).toBeVisible();
    await expect(page.getByRole("region", { name: "连接" }).getByRole("heading", { name: "连接信息" })).toBeVisible();

    await mkdir(screenshotDirectory, { recursive: true });
    await page.screenshot({
      path: path.join(screenshotDirectory, "connection-390x844.png"),
      fullPage: true,
    });

    await tabs.getByRole("link", { name: "地址与地点" }).click();
    const addresses = page.getByRole("region", { name: "地址与地点" });
    await expect(addresses.getByText("同一台机器上的其他账号")).toBeVisible();
    await expect(addresses.getByText("db-1")).toBeVisible();
    await expect(addresses.getByText("家A局域网").first()).toBeVisible();
    await page.screenshot({
      path: path.join(screenshotDirectory, "addresses-390x844.png"),
      fullPage: true,
    });

    await tabs.getByRole("link", { name: "授权" }).click();
    const authorization = page.getByRole("region", { name: "授权" });
    await expect(authorization.getByText("MATRIX_TOKEN", { exact: true })).toBeVisible();
    await page.screenshot({
      path: path.join(screenshotDirectory, "authorization-390x844.png"),
      fullPage: true,
    });

    await tabs.getByRole("link", { name: "软件" }).click();
    const software = page.getByRole("region", { name: "软件" });
    await expect(software.getByText("采集状态")).toBeVisible();
    await page.screenshot({
      path: path.join(screenshotDirectory, "software-390x844.png"),
      fullPage: true,
    });

    // 更改地点：先当场新建，再选中新地点保存。
    await tabs.getByRole("link", { name: "地址与地点" }).click();
    await addresses.getByRole("link", { name: "更改" }).click();
    await expect(addresses.getByRole("radiogroup", { name: "选择地点" })).toBeVisible();
    await addresses.getByRole("link", { name: "新建地点" }).click();
    await page.getByLabel("名称").fill("浏览器测试地点");
    await page.getByRole("button", { name: "保存" }).click();
    await page.getByRole("heading", { name: "web-1", exact: true }).waitFor();
    await page.getByRole("radio", { name: "浏览器测试地点" }).check();
    await page.getByRole("button", { name: "保存地点" }).click();
    await expect(page.getByText("已保存机器地点。")).toBeVisible();
    await expect(page.locator(".location-value")).toHaveText("浏览器测试地点");

    // 添加地址：机房局域网还没有这台机器的地址。
    await page.getByText("＋ 添加地址").click();
    await page.getByLabel("地址", { exact: true }).fill("10.0.0.99");
    await page.getByRole("button", { name: "添加", exact: true }).click();
    await expect(page.getByText("已保存地址。")).toBeVisible();
    await expect(
      page.locator(".address-row", { hasText: "机房局域网" }).getByText("10.0.0.99"),
    ).toBeVisible();

    // 授权机密项：开关先进入确认页，并显示客户端实际取得途径变化。
    await gotoUIPath(page, "/ui/servers/web-1?tab=authorization");
    await page
      .locator(".auth-row", { hasText: "MATRIX_TOKEN" })
      .locator(".switch-control")
      .click();
    await expect(page.getByRole("heading", { name: "机密项授权预览" })).toBeVisible();
    const impact = page.getByRole("region", { name: "实际能拿到它的客户端如何变化" });
    await expect(impact.getByRole("link", { name: "laptop-client" })).toBeVisible();
    await expect(impact.getByText("提交后经 web-1")).toBeVisible();
    await page.screenshot({
      path: path.join(screenshotDirectory, "secret-confirm-390x844.png"),
      fullPage: true,
    });
    await page.getByRole("button", { name: "确认保存授权" }).click();
    await expect(page.getByText("已保存授权变更。")).toBeVisible();
  } finally {
    await ui.close();
  }
});
