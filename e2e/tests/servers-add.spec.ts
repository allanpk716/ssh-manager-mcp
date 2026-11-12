import { readFile } from "node:fs/promises";
import path from "node:path";
import { expect, openStandaloneUI, startStandaloneUIWithTopology, test } from "../helpers";

test("从页头新增服务器并复用已有设备的网络成员", async ({ page }) => {
  const ui = await startStandaloneUIWithTopology();
  try {
    await openStandaloneUI(page, ui);

    await page.getByRole("link", { name: "添加", exact: true }).click();
    await page.getByRole("link", { name: /服务器/ }).click();
    await expect(page.getByRole("heading", { name: "新增服务器" })).toBeVisible();

    await page.getByLabel("名称", { exact: true }).fill("same-device");
    await page.getByLabel("用户", { exact: true }).fill("deploy");
    await page.getByLabel("端口", { exact: true }).fill("22");
    await page.getByLabel("登录凭据").selectOption("password");
    await page.getByLabel("密码", { exact: true }).fill("browser-new-secret");
    await page.getByRole("radio", { name: "已有设备" }).check();
    await page.locator("#add-existing-device").selectOption("工作站");
    await expect(page.getByLabel("站点", { exact: true })).toBeHidden();
    await expect(page.getByLabel("物理局域网", { exact: true })).toBeHidden();
    await page.locator("details", { hasText: "profile 授权" }).locator("summary").click();
    await page.locator('input[name="profile_id"]').check();

    await page.getByRole("button", { name: "保存并进入待连接测试" }).click();
    await expect(page.getByRole("heading", { name: /已保存，连接测试/ })).toBeVisible();
    await expect(page.getByText("same-device")).toBeVisible();
    await expect(page.getByText("deploy@192.168.1.20:22")).toBeVisible();
    await expect(page.getByText("工作站")).toBeVisible();
    await expect(page.locator("#content dd").filter({ hasText: "运维" })).toBeVisible();
    await expect(page.getByText("未验证", { exact: true })).toBeVisible();
    await expect(page.getByText("shell 未验证", { exact: false })).toBeVisible();
    await expect(page.locator("#content")).not.toContainText("browser-new-secret");
  } finally {
    await ui.close();
  }
});

test("手机从服务器列表新增并粘贴私钥", async ({ page, isMobile }, testInfo) => {
  testInfo.skip(!isMobile, "这个用例只运行手机视口");
  const ui = await startStandaloneUIWithTopology();
  try {
    await openStandaloneUI(page, ui);
    await page.getByRole("link", { name: "新增服务器" }).click();

    await page.getByLabel("名称", { exact: true }).fill("mobile-web");
    await page.getByLabel("用户", { exact: true }).fill("deploy");
    await page.getByLabel("登录凭据").selectOption("key");
    const key = await readFile(path.join(__dirname, "fixtures", "encrypted_ec.pem"), "utf8");
    await page.getByLabel("私钥内容").fill(key);
    await page.getByLabel("设备名").fill("手机登记设备");
    await page.getByLabel("物理局域网", { exact: true }).selectOption("家A局域网");
    await page.getByLabel("物理局域网地址").fill("192.168.1.40");
    await page.getByLabel("虚拟局域网", { exact: true }).first().selectOption("虚拟网");
    await page.getByLabel("地址", { exact: true }).first().fill("100.64.0.40");
    await page.locator("details", { hasText: "profile 授权" }).locator("summary").click();
    await page.locator('input[name="profile_id"]').check();
    await page.getByRole("button", { name: "保存并进入待连接测试" }).click();

    await expect(page.getByRole("heading", { name: /已保存，连接测试/ })).toBeVisible();
    await expect(page.getByText("mobile-web")).toBeVisible();
    await expect(page.getByText("deploy@192.168.1.40:22")).toBeVisible();
    await expect(page.getByText("手机登记设备")).toBeVisible();
    await expect(page.locator("#content dd").filter({ hasText: "运维" })).toBeVisible();
    await expect(page.getByText("未验证", { exact: true })).toBeVisible();
    await expect(page.getByText("shell 未验证", { exact: false })).toBeVisible();
    await expect(page.locator("#content")).not.toContainText(key);
    const overflow = await page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth);
    expect(overflow).toBeLessThanOrEqual(2);
    await expect(page.locator("canvas")).toHaveCount(0);
  } finally {
    await ui.close();
  }
});
