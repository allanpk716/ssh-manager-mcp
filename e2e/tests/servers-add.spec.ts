import { mkdtempSync } from "node:fs";
import os from "node:os";
import path from "node:path";
import { expect, openStandaloneUI, startStandaloneUIWithTopology, startTestSSHServer, test } from "../helpers";

const screenshotDir = "/Users/allan716/.cache/implement-spec-split/runs/ssh-manager-mcp-294-20261119-0809/impl-298/screenshots";

test("手机分步新增服务器并上传私钥", async ({ page, isMobile }, testInfo) => {
  testInfo.skip(!isMobile, "这个用例按 390 视口验证手机流程");
  const runDir = mkdtempSync(path.join(os.tmpdir(), "sshmgr-server-add-"));
  const sshd = await startTestSSHServer(path.join(runDir, "test-key.pem"));
  const ui = await startStandaloneUIWithTopology();
  try {
    await openStandaloneUI(page, ui);
    await page.getByRole("link", { name: "新增服务器" }).click();
    await expect(page.getByRole("heading", { name: "新增服务器" })).toBeVisible();

    const [host, port] = sshd.address.split(":");
    await page.getByLabel("名称", { exact: true }).fill("mobile-key-server");
    await page.getByLabel("用户", { exact: true }).fill("deploy");
    await page.getByLabel("端口", { exact: true }).fill(port);
    await page.getByRole("radio", { name: "私钥" }).check();
    await page.getByLabel("私钥文件").setInputFiles(sshd.privateKeyPath);
    await page.locator(".address-row select").selectOption("other");
    await page.locator(".address-row input[name='address']").fill(host);
    await page.screenshot({ path: `${screenshotDir}/mobile-server-step-1.png`, fullPage: true });

    await page.getByRole("button", { name: "测试连接并下一步" }).click();
    await expect(page.getByRole("heading", { name: "连接测试完成" })).toBeVisible({ timeout: 20_000 });
    await expect(page.getByText(/首次连接已记录主机密钥锚|主机密钥指纹/)).toBeVisible();
    await page.screenshot({ path: `${screenshotDir}/mobile-server-connection-test.png`, fullPage: true });
    await page.getByRole("link", { name: "继续选择地点" }).click();

    await page.getByLabel("家A", { exact: true }).check();
    await page.getByRole("button", { name: "使用选中地点" }).click();
    await expect(page.getByRole("heading", { name: "选择 Profile" })).toBeVisible();
    await page.getByRole("checkbox", { name: /运维/ }).check();
    await expect(page.getByText("当前勾选将对 0 个客户端可见")).toBeVisible();
    await page.screenshot({ path: `${screenshotDir}/mobile-server-step-3.png`, fullPage: true });
    await page.getByRole("button", { name: "完成新增" }).click();

    await expect(page.getByRole("heading", { name: /已保存，连接测试完成/ })).toBeVisible();
    await expect(page.getByText("已验证")).toBeVisible();
    await page.screenshot({ path: `${screenshotDir}/mobile-server-result.png`, fullPage: true });
  } finally {
    await ui.close();
    await sshd.close();
  }
});

test("手机输入已登记地址时识别同一台机器", async ({ page, isMobile }, testInfo) => {
  testInfo.skip(!isMobile, "这个用例按 390 视口验证手机流程");
  const ui = await startStandaloneUIWithTopology();
  try {
    await openStandaloneUI(page, ui);
    await page.getByRole("link", { name: "新增服务器" }).click();
    await page.locator(".address-row select").selectOption("家A局域网");
    await page.locator(".address-row input[name='address']").fill("192.168.1.20");
    await expect(page.getByText("这是 工作站 上的另一个账号吗？")).toBeVisible();
    await page.getByRole("radio", { name: /是，挂到同一台机器/ }).check();
    await page.screenshot({ path: `${screenshotDir}/mobile-same-machine.png`, fullPage: true });
  } finally {
    await ui.close();
  }
});

test("手机服务器列表多选后批量加入 profile", async ({ page, isMobile }, testInfo) => {
  testInfo.skip(!isMobile, "这个用例按 390 视口验证手机流程");
  const ui = await startStandaloneUIWithTopology();
  try {
    await openStandaloneUI(page, ui);
    await page.getByRole("checkbox", { name: "选择 db-1" }).check();
    await page.getByRole("checkbox", { name: "选择 old-1" }).check();
    await expect(page.getByText("已选 2 台")).toBeVisible();
    await page.getByRole("button", { name: "加入 profile" }).click();
    await page.getByRole("link", { name: /运维/ }).click();

    await expect(page.getByRole("heading", { name: "调整 运维" })).toBeVisible();
    await expect(page.locator(".selection-row", { hasText: "db-1" }).locator("input")).toBeChecked();
    await expect(page.locator(".selection-row", { hasText: "old-1" }).locator("input")).toBeChecked();
    await page.screenshot({ path: `${screenshotDir}/mobile-batch-profile-selection.png`, fullPage: true });
    await page.getByRole("button", { name: "下一步" }).click();
    await page.getByRole("button", { name: "确认保存授权" }).click();
    await expect(page.getByText(/已保存/)).toBeVisible({ timeout: 10_000 });
    await page.screenshot({ path: `${screenshotDir}/mobile-batch-profile-result.png`, fullPage: true });
  } finally {
    await ui.close();
  }
});
