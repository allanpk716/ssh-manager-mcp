import { mkdir } from "node:fs/promises";
import path from "node:path";
import { expect, openAuthorityUI, startAuthorityUI, test } from "../helpers";

const screenshotDirectory =
  "/Users/allan716/.cache/implement-spec-split/runs/ssh-manager-mcp-294-20261119-0809/impl-300/screenshots";

async function prepareMobileDarkPage(page: import("@playwright/test").Page): Promise<void> {
  await page.setViewportSize({ width: 390, height: 844 });
  await page.emulateMedia({ colorScheme: "dark" });
}

test("手机视口在配对列表一次完成批准", async ({ page }) => {
  const ui = await startAuthorityUI("browser-test-password", "normal");
  await prepareMobileDarkPage(page);
  try {
    await openAuthorityUI(page, ui, ui.password);
    await page.getByRole("navigation", { name: "一级导航" }).getByRole("link", { name: "客户端" }).click();
    await expect(page.getByRole("heading", { name: "客户端" })).toBeVisible();
    await page.getByRole("link", { name: "去审批" }).click();
    await expect(page).toHaveURL(/\/ui\/clients\/pairing$/);
    const request = page.locator(".pairing-request", { hasText: "端口映射客户端" });
    await expect(request.getByText("端口映射客户端")).toBeVisible();
    await expect(request.getByText("https://172.18.200.47:18178")).toBeVisible();
    await expect(request.getByText("246810")).toBeVisible();
    const profileSelect = request.getByLabel("批准到 profile");
    expect(await profileSelect.evaluate((element) => element.value !== "")).toBe(true);
    await expect(request.getByRole("checkbox", { name: "我已逐位比对三项一致" })).toBeVisible();
    await mkdir(screenshotDirectory, { recursive: true });
    await page.screenshot({
      path: path.join(screenshotDirectory, "pairing-normal-390x844.png"),
      fullPage: true,
    });
    await request.getByRole("checkbox", { name: "我已逐位比对三项一致" }).check();
    await request.getByRole("button", { name: "批准" }).click();
    await expect(page.getByText("已批准配对请求。请让客户端在完成窗口内继续操作。")).toBeVisible();
    await expect(page.getByRole("button", { name: "批准" })).toHaveCount(0);
  } finally {
    await ui.close();
  }
});

test("手机视口对异常地址只显示拒绝", async ({ page }) => {
  const ui = await startAuthorityUI("browser-test-password", "foreign");
  await prepareMobileDarkPage(page);
  try {
    await openAuthorityUI(page, ui, ui.password);
    await page.getByRole("navigation", { name: "一级导航" }).getByRole("link", { name: "客户端" }).click();
    await page.getByRole("link", { name: "去审批" }).click();
    const request = page.locator(".pairing-request", { hasText: "陌生地址客户端" });
    await expect(request.getByText("https://203.0.113.9:7878")).toBeVisible();
    await expect(
      request.getByText(
        "这条请求连接的地址不是本机，也不在已登记的对外地址里，可能有人在中间转发。建议拒绝。",
      ),
    ).toBeVisible();
    await expect(request.getByRole("button", { name: "批准" })).toHaveCount(0);
    await expect(request.getByRole("button", { name: "拒绝" })).toBeVisible();
    await mkdir(screenshotDirectory, { recursive: true });
    await page.screenshot({
      path: path.join(screenshotDirectory, "pairing-foreign-390x844.png"),
      fullPage: true,
    });
  } finally {
    await ui.close();
  }
});
