import { mkdir } from "node:fs/promises";
import type { Page } from "@playwright/test";
import { expect, startStandaloneUIWithTopology, test } from "../helpers";

const screenshotDirectory =
  "/Users/allan716/.cache/implement-spec-split/runs/ssh-manager-mcp-294-20261119-0809/impl-302/screenshots";

async function prepareMobilePage(page: Page): Promise<void> {
  await page.setViewportSize({ width: 390, height: 844 });
}

async function openMoreMenu(page: Page): Promise<void> {
  await page.getByRole("navigation", { name: "一级导航" }).getByRole("link", { name: "更多" }).click();
  await page.getByRole("heading", { name: "更多" }).waitFor();
}

function locationNode(page: Page, path: string) {
  return page
    .getByText(`完整路径：${path}`, { exact: true })
    .locator("xpath=ancestor::details[contains(@class, 'tree-node')][1]");
}

async function createLocation(page: Page, name: string, parent: string): Promise<void> {
  await page.getByRole("link", { name: "新建地点" }).click();
  await page.getByRole("heading", { name: "新建地点" }).waitFor();
  await page.getByLabel("名称", { exact: true }).fill(name);
  if (parent === "") {
    await page.getByLabel("无上级（顶级地点）").check();
  } else {
    await page.getByLabel(new RegExp(parent)).check();
  }
  await page.getByRole("button", { name: "保存" }).click();
  await page.getByRole("heading", { name: "地点与网络" }).waitFor();
}

test("390 视口管理三级地点、移动、删除守卫与网络覆盖地点", async ({ page, isMobile }) => {
  test.skip(!isMobile, "本用例验证 390 手机视口的信息架构");
  const ui = await startStandaloneUIWithTopology();
  await prepareMobilePage(page);
  try {
    await mkdir(screenshotDirectory, { recursive: true });
    await page.goto(ui.startupURL);
    await page.getByRole("heading", { name: "状态总览" }).waitFor();
    await openMoreMenu(page);
    await expect(page.getByRole("heading", { name: "资产" })).toBeVisible();
    await expect(page.getByRole("heading", { name: "运维" })).toBeVisible();
    await page.screenshot({ path: `${screenshotDirectory}/more-menu.png`, fullPage: true });

    await page.getByRole("link", { name: "地点与网络" }).click();
    await page.getByRole("heading", { name: "地点与网络" }).waitFor();
    await page.screenshot({ path: `${screenshotDirectory}/locations-empty.png`, fullPage: true });

    await createLocation(page, "公司", "");
    const company = locationNode(page, "公司");
    await company.getByRole("link", { name: "新建子地点" }).first().click();
    await page.getByLabel("名称", { exact: true }).fill("一厂区");
    await page.getByLabel(/公司/).check();
    await page.getByRole("button", { name: "保存" }).click();
    await page.getByRole("heading", { name: "地点与网络" }).waitFor();

    const factory = locationNode(page, "公司 / 一厂区");
    await factory.getByRole("link", { name: "新建子地点" }).first().click();
    await page.getByLabel("名称", { exact: true }).fill("3 号楼");
    await page.getByLabel(/一厂区/).check();
    await page.getByRole("button", { name: "保存" }).click();
    await page.getByRole("heading", { name: "地点与网络" }).waitFor();
    await expect(page.getByText("完整路径：公司 / 一厂区 / 3 号楼")).toBeVisible();
    await page.screenshot({ path: `${screenshotDirectory}/locations-three-level.png`, fullPage: true });

    const building = locationNode(page, "公司 / 一厂区 / 3 号楼");
    await building.getByRole("link", { name: "移动到其他上级" }).click();
    await page.getByRole("heading", { name: "移动地点" }).waitFor();
    await page.getByLabel("无上级（顶级地点）").check();
    await page.getByRole("button", { name: "保存" }).click();
    await page.getByRole("heading", { name: "地点与网络" }).waitFor();
    await expect(page.getByText("完整路径：3 号楼")).toBeVisible();
    await page.screenshot({ path: `${screenshotDirectory}/locations-moved.png`, fullPage: true });

    const companyNode = locationNode(page, "公司");
    await expect(companyNode).toContainText("还有下级地点：一厂区");
    await expect(companyNode.locator(".node-actions").first().getByRole("link", { name: "删除" })).toHaveCount(0);
    await page.goto(new URL("/ui/more/locations/delete?site=" + encodeURIComponent("公司"), page.url()).toString());
    await page.getByRole("heading", { name: "删除地点" }).waitFor();
    await expect(page.getByText(/还有下级地点：一厂区/)).toBeVisible();
    await expect(page.getByRole("button", { name: "确认删除" })).toHaveCount(0);
    await page.screenshot({ path: `${screenshotDirectory}/location-delete-refused.png`, fullPage: true });

    await page.getByRole("link", { name: "返回地点与网络" }).click();
    await page.getByRole("link", { name: "网络" }).click();
    await page.locator(".network-row", { hasText: "家A局域网" }).getByRole("link", { name: "编辑" }).click();
    await page.getByRole("heading", { name: "编辑网络" }).waitFor();
    await page.getByLabel(/机房/).check();
    await page.getByRole("button", { name: "保存" }).click();
    await page.getByRole("heading", { name: "地点与网络" }).waitFor();
    await page.getByRole("link", { name: "网络" }).click();
    const editedNetwork = page.locator(".network-row", { hasText: "家A局域网" });
    await expect(editedNetwork).toContainText("覆盖地点");
    await expect(editedNetwork).toContainText("机房");
    await page.screenshot({ path: `${screenshotDirectory}/network-coverage-edited.png`, fullPage: true });

    const overflow = await page.evaluate(
      () => document.documentElement.scrollWidth - document.documentElement.clientWidth,
    );
    expect(overflow).toBeLessThanOrEqual(2);
  } finally {
    await ui.close();
  }
});

test("网络表单说明网段用途并在非法输入后保留字段", async ({ page }) => {
  const ui = await startStandaloneUIWithTopology();
  try {
    await page.goto(ui.startupURL);
    await page.getByRole("heading", { name: "状态总览" }).waitFor();
    await openMoreMenu(page);
    await page.getByRole("link", { name: "地点与网络" }).click();
    await page.getByRole("heading", { name: "地点与网络" }).waitFor();
    await page.getByRole("link", { name: "新建物理局域网" }).click();
    await page.getByRole("heading", { name: "新建网络" }).waitFor();

    const rangePurpose = page.getByText("用来提示哪些地址属于这个网络，不会自动归入。");
    await expect(rangePurpose).toBeVisible();
    await page.getByLabel("名称", { exact: true }).fill("浏览器测试网络");
    await page.getByLabel("家A", { exact: true }).check();
    await page.getByLabel("网段", { exact: true }).fill("192.168.100.1/24");
    await page.getByLabel("标称带宽（Mbps）").fill("1000");
    await page.getByLabel("按流量计费").check();
    await page.getByRole("button", { name: "保存" }).click();

    await expect(page.getByText("网段必须是网络地址，主机位必须为零")).toBeVisible();
    await expect(page.getByLabel("名称", { exact: true })).toHaveValue("浏览器测试网络");
    await expect(page.getByLabel("家A", { exact: true })).toBeChecked();
    await expect(page.getByLabel("网段", { exact: true })).toHaveValue("192.168.100.1/24");
    await expect(page.getByLabel("标称带宽（Mbps）")).toHaveValue("1000");
    await expect(page.getByLabel("按流量计费")).toBeChecked();

    await page.getByRole("link", { name: "返回地点与网络" }).click();
    await page.getByRole("heading", { name: "地点与网络" }).waitFor();
    await page.locator(".network-row", { hasText: "家A局域网" }).getByRole("link", { name: "编辑" }).click();
    await page.getByRole("heading", { name: "编辑网络" }).waitFor();
    await expect(rangePurpose).toBeVisible();
    await page.getByLabel("网段", { exact: true }).fill("not-a-network");
    await page.getByRole("button", { name: "保存" }).click();

    await expect(page.getByText("网段必须是合法的 IPv4 或 IPv6 网络地址")).toBeVisible();
    await expect(page.getByLabel("名称", { exact: true })).toHaveValue("家A局域网");
    await expect(page.getByLabel("家A", { exact: true })).toBeChecked();
    await expect(page.getByLabel("网段", { exact: true })).toHaveValue("not-a-network");
    await expect(page.getByLabel("标称带宽（Mbps）")).toHaveValue("1000");
    await expect(page.getByLabel("按流量计费")).toBeChecked();
  } finally {
    await ui.close();
  }
});

