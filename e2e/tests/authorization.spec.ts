import { mkdir } from "node:fs/promises";
import path from "node:path";
import { expect, gotoUIPath, openStandaloneUI, test } from "../helpers";

const screenshotDirectory =
  "/Users/allan716/.cache/implement-spec-split/runs/ssh-manager-mcp-294-20261119-0809/impl-301/screenshots";

async function prepareMobilePage(page: import("@playwright/test").Page): Promise<void> {
  await page.setViewportSize({ width: 390, height: 844 });
}

test("机密项可搜索筛选批量调整服务器并经确认页保存", async ({ page, standaloneUI }) => {
  await openStandaloneUI(page, standaloneUI);
  await prepareMobilePage(page);

  await gotoUIPath(page, "/ui/authorization/secrets");
  await expect(page.getByRole("heading", { name: "机密项" })).toBeVisible();
  await page.getByRole("link", { name: /MATRIX_TOKEN/ }).click();
  await expect(page.getByRole("heading", { name: "MATRIX_TOKEN" })).toBeVisible();
  await page.getByRole("link", { name: "调整服务器" }).click();

  await expect(page.getByRole("heading", { name: "调整 MATRIX_TOKEN 的服务器" })).toBeVisible();
  const selectAll = page.getByRole("checkbox", { name: /全选当前筛选结果/ });
  await expect(selectAll).toHaveAttribute("aria-checked", "mixed");

  await page.getByLabel("搜索服务器").fill("web-1");
  await expect(page.locator(".selection-row", { hasText: "db-1" })).toBeHidden();
  await expect(page.locator(".selection-row", { hasText: "web-1" })).toBeVisible();
  await expect(page.locator("[data-selection-visible-count]")).toHaveText("1 台");
  await page.getByLabel("搜索服务器").fill("");

  await page.locator(".selection-row", { hasText: "web-1" }).getByRole("checkbox").check();
  await expect(page.getByText("新增 1 台、移除 0 台")).toBeVisible();
  await expect(selectAll).toHaveAttribute("aria-checked", "mixed");
  await selectAll.click();
  await expect(page.getByText("新增 2 台、移除 0 台")).toBeVisible();
  await expect(selectAll).toHaveAttribute("aria-checked", "true");
  await page.getByRole("button", { name: "下一步" }).click();

  await expect(page.getByRole("heading", { name: "机密项授权预览" })).toBeVisible();
  await expect(page.getByText("新增 2 个授权，移除 0 个授权。")).toBeVisible();
  const impact = page.getByRole("region", { name: "实际能拿到它的客户端如何变化" });
  await expect(impact.getByRole("link", { name: "laptop-client" })).toBeVisible();
  await expect(impact.getByText(/提交后经 (web-1、old-1|old-1、web-1)/)).toBeVisible();
  await mkdir(screenshotDirectory, { recursive: true });
  await page.screenshot({
    path: path.join(screenshotDirectory, "secret-authorization-confirm-390x844.png"),
    fullPage: true,
  });
  await page.getByRole("button", { name: "确认保存授权" }).click();
  await expect(page.getByText("已保存授权变更。")).toBeVisible();
});

test("新建机密项备注为空时不能提交", async ({ page, standaloneUI }) => {
  await openStandaloneUI(page, standaloneUI);
  await prepareMobilePage(page);

  await gotoUIPath(page, "/ui/authorization/secrets");
  await page.getByRole("link", { name: "新建机密项" }).click();
  await page.getByLabel("名称").fill("EMPTY_NOTE_TOKEN");
  await page.getByLabel("值", { exact: true }).fill("value-without-note");
  const create = page.getByRole("button", { name: "创建" });
  await expect(create).toBeDisabled();
  await page.getByLabel("备注").fill("浏览器测试必填备注");
  await expect(create).toBeEnabled();
});

test("共享存储详情显示实际能拿到它的客户端与途径", async ({ page, sharesStandaloneUI }) => {
  await openStandaloneUI(page, sharesStandaloneUI);
  await prepareMobilePage(page);

  await gotoUIPath(page, "/ui/authorization/shares");
  await page.getByRole("link", { name: "datasets" }).click();
  await expect(page.getByRole("heading", { name: "datasets" })).toBeVisible();
  const clients = page.getByRole("region", { name: "实际能拿到它的客户端" });
  await expect(clients.getByRole("link", { name: "laptop-client" })).toBeVisible();
  await expect(clients.getByText("经 web-1")).toBeVisible();
  await mkdir(screenshotDirectory, { recursive: true });
  await page.screenshot({
    path: path.join(screenshotDirectory, "share-detail-clients-390x844.png"),
    fullPage: true,
  });
});
