import { mkdir } from "node:fs/promises";
import path from "node:path";
import {
  expect,
  gotoUIPath,
  startStandaloneUIWithTopology,
  startTestSSHServer,
  test,
} from "../helpers";

// serverCard 定位某一台服务器所在的卡片，再在卡片内找链接与状态行，避免
// 同名链接互相干扰。
function serverCard(page: import("@playwright/test").Page, name: string) {
  return page.locator("[data-server-card]", { has: page.getByRole("link", { name, exact: true }) });
}

// usePrivateKey 把第一台批量测试条目改成私钥登录，指向本次启动的 SSH 测
// 试服务；后两台保持失败端口，浏览器层不重复验证 SSH 细节。
async function usePrivateKey(
  page: import("@playwright/test").Page,
  port: string,
  privateKeyPath: string,
): Promise<void> {
  await page.getByRole("region", { name: "连接" }).getByRole("link", { name: "编辑" }).click();
  await page.locator("#edit-host").fill("127.0.0.1");
  await page.locator("#edit-port").fill(port);
  await page.locator("#edit-credential").selectOption("key");
  await page.locator("#edit-key-file").setInputFiles(privateKeyPath);
  await page.getByRole("button", { name: "保存连接信息" }).click();
  await page.getByRole("heading", { name: "batch-connection-1", exact: true }).waitFor();
}

test("批量测试连接逐卡显示进行中与结果并在刷新后保留", async ({ page }) => {
  const keyDirectory = "/tmp/sshmgr-servers-batch-connection-key";
  await mkdir(keyDirectory, { recursive: true });
  const sshd = await startTestSSHServer(path.join(keyDirectory, "batch-key.pem"), {
    handshakeDelayMilliseconds: 800,
  });
  const [, okPort] = sshd.address.split(":");
  const ui = await startStandaloneUIWithTopology(undefined, undefined, {
    SSHMGR_E2E_BATCH_CONNECTION_PORTS: [okPort, "9", "10"].join(","),
  });
  try {
    await page.goto(ui.startupURL);
    await page.getByRole("heading", { name: "状态总览" }).waitFor();
    await gotoUIPath(page, "/ui/servers/");

    const names = ["batch-connection-1", "batch-connection-2", "batch-connection-3"];
    await serverCard(page, names[0]).getByRole("link", { name: "详情" }).click();
    await page.getByRole("heading", { name: names[0], exact: true }).waitFor();
    await usePrivateKey(page, okPort, sshd.privateKeyPath);
    await page.getByRole("link", { name: "返回服务器列表" }).click();
    await page.getByRole("heading", { name: /^服务器/ }).first().waitFor();

    await page.getByRole("button", { name: "多选" }).click();
    for (const name of names) {
      await page.getByLabel(`选择 ${name}`).check();
    }
    const testButton = page.getByRole("button", { name: "测试连接" });
    await expect(testButton).toBeEnabled();
    await testButton.click();

    await expect(page.locator(".server-test-line.is-running").first()).toBeVisible({ timeout: 3_000 });
    const okCard = serverCard(page, names[0]);
    const downCards = [serverCard(page, names[1]), serverCard(page, names[2])];
    await expect(okCard.locator(".server-test-line.is-succeeded")).toContainText("已连上", {
      timeout: 20_000,
    });
    await expect(okCard.locator(".server-test-line.is-succeeded")).toContainText("127.0.0.1");
    for (const card of downCards) {
      await expect(card.locator(".server-test-line.is-failed")).toContainText("连不上", {
        timeout: 20_000,
      });
    }
    await expect(page.locator("[data-server-batch-connection-summary]")).toContainText(
      "连接测试完成：3 台，已连上 1 台，连不上 2 台",
    );
    await expect(testButton).toBeEnabled();
    await expect(testButton).toContainText("测试连接");

    await page.reload();
    await expect(okCard.locator(".server-test-line.is-succeeded")).toContainText("已连上");
    await expect(downCards[0].locator(".server-test-line.is-failed")).toContainText("连不上");
    await expect(downCards[1].locator(".server-test-line.is-failed")).toContainText("连不上");
    await expect(page.locator("[data-server-batch-connection-summary]")).toHaveCount(0);
  } finally {
    await ui.close();
    await sshd.close();
  }
});
