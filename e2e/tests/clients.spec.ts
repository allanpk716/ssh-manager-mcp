import { mkdir } from "node:fs/promises";
import path from "node:path";
import type { Page } from "@playwright/test";
import { expect, gotoUIPath, openAuthorityUI, startAuthorityUI, test } from "../helpers";

const screenshotDirectory =
  "/Users/allan716/.cache/implement-spec-split/runs/ssh-manager-mcp-294-20261119-0809/impl-300/screenshots";

async function prepareMobilePage(page: Page): Promise<void> {
  await page.setViewportSize({ width: 390, height: 844 });
}

async function submitAfterReverify(page: Page, buttonName: string, password: string): Promise<void> {
  await page.waitForTimeout(1100);
  await page.getByRole("button", { name: buttonName }).click();
  await expect(page).toHaveURL(/\/ui\/reverify/);
  await page.getByLabel("管理员口令").fill(password);
  await page.getByRole("button", { name: "重新验证" }).click();
  await expect(page).not.toHaveURL(/\/ui\/reverify/);
  await page.getByRole("button", { name: buttonName }).click();
}

test("390 视口完成更换 profile、选择机器与状态展示", async ({ page }) => {
  const ui = await startAuthorityUI("browser-test-password", "", true);
  await prepareMobilePage(page);
  try {
    await openAuthorityUI(page, ui, ui.password);
    await page.getByRole("navigation", { name: "一级导航" }).getByRole("link", { name: "客户端" }).click();
    await expect(page).toHaveURL(/\/ui\/clients\/$/);
    await expect(page.getByRole("heading", { name: "客户端" })).toBeVisible();
    const clientCard = page.locator(".client-card", { hasText: "laptop-client" });
    await expect(clientCard.getByText("运维 · 2 台服务器")).toBeVisible();
    await expect(clientCard.getByText("机器未关联")).toBeVisible();
    await expect(clientCard.getByText("手动签发")).toBeVisible();
    await clientCard.click();

    await page.getByRole("link", { name: "更换" }).click();
    await page.getByRole("link", { name: /数据库 · 1 台服务器/ }).click();
    await expect(page.getByText("运维 → 数据库")).toBeVisible();
    await expect(page.getByText("将新增可见 1 台：db-1")).toBeVisible();
    await expect(page.getByText("将失去可见 2 台：web-1、old-1")).toBeVisible();
    await expect(page.getByText("将新增可用机密项 1 项：MATRIX_TOKEN")).toBeVisible();
    await expect(page.getByText("将失去可用机密项 1 项：WEB_TOKEN")).toBeVisible();
    await mkdir(screenshotDirectory, { recursive: true });
    await page.screenshot({ path: path.join(screenshotDirectory, "client-profile-preview-390x844.png"), fullPage: true });
    await submitAfterReverify(page, "确认更换", ui.password);
    await expect(page.getByText("数据库 · 它能看到 1 台服务器")).toBeVisible();
    await expect(page.getByText("db-1")).toBeVisible();

    await page.getByRole("link", { name: "选择机器" }).click();
    await expect(page.getByRole("heading", { name: "选择机器" })).toBeVisible();
    await page.getByRole("radio", { name: /工作站/ }).check();
    await mkdir(screenshotDirectory, { recursive: true });
    await page.screenshot({ path: path.join(screenshotDirectory, "client-machine-390x844.png"), fullPage: true });
    await submitAfterReverify(page, "确认选择", ui.password);
    await expect(page.getByText("机器 工作站")).toBeVisible();
    await expect(page.getByText(/最近采集 .* · 3 个软件/)).toBeVisible();
  } finally {
    await ui.close();
  }
});

test("390 视口确认移除客户端", async ({ page }) => {
  const ui = await startAuthorityUI("browser-test-password", "", true);
  await prepareMobilePage(page);
  try {
    await openAuthorityUI(page, ui, ui.password);
    await page.getByRole("navigation", { name: "一级导航" }).getByRole("link", { name: "客户端" }).click();
    await page.locator(".client-card", { hasText: "laptop-client" }).click();
    await page.getByRole("link", { name: "移除客户端" }).click();
    await expect(page.getByRole("heading", { name: "移除客户端 laptop-client" })).toBeVisible();
    await page.getByRole("checkbox", { name: "确认移除" }).check();
    await mkdir(screenshotDirectory, { recursive: true });
    await page.screenshot({ path: path.join(screenshotDirectory, "client-remove-390x844.png"), fullPage: true });
    await submitAfterReverify(page, "确认移除", ui.password);
    await expect(page).toHaveURL(/\/ui\/clients\/\?notice=removed$/);
    await expect(page.locator(".client-card", { hasText: "laptop-client" })).toHaveCount(0);
  } finally {
    await ui.close();
  }
});

test("高级页面使用新名称并保留旧入口重定向", async ({ page }) => {
  const ui = await startAuthorityUI("browser-test-password");
  await prepareMobilePage(page);
  try {
    await openAuthorityUI(page, ui, ui.password);
    await gotoUIPath(page, "/ui/access/projects");
    await expect(page).toHaveURL(/\/ui\/ops\/access-tokens$/);
    await expect(page.getByRole("heading", { name: "MCP 访问令牌" })).toBeVisible();
    await gotoUIPath(page, "/ui/access/device-codes");
    await expect(page).toHaveURL(/\/ui\/ops\/clients\/new$/);
    await expect(page.getByRole("heading", { name: "手动添加客户端" })).toBeVisible();
  } finally {
    await ui.close();
  }
});
