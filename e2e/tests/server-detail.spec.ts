import { mkdir } from "node:fs/promises";
import path from "node:path";
import {
  expect,
  gotoUIPath,
  reserveFreePort,
  startSilentSSHServer,
  startStandaloneUIWithTopology,
  startTestSSHServer,
  test,
} from "../helpers";

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
      page.locator(".address-card-row", { hasText: "机房局域网" }).getByText("10.0.0.99"),
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

const connectionTargetPath = "/ui/servers/connection-target?tab=connection";

async function openConnectionTarget(
  page: import("@playwright/test").Page,
  ui: { startupURL: string },
): Promise<void> {
  await page.goto(ui.startupURL);
  await page.getByRole("heading", { name: "状态总览" }).waitFor();
  await gotoUIPath(page, connectionTargetPath);
  await page.getByRole("heading", { name: "connection-target", exact: true }).waitFor();
}

async function usePrivateKey(
  page: import("@playwright/test").Page,
  host: string,
  port: string,
  privateKeyPath: string,
): Promise<void> {
  await page.getByRole("region", { name: "连接" }).getByRole("link", { name: "编辑" }).click();
  await page.locator("#edit-host").fill(host);
  await page.locator("#edit-port").fill(port);
  await page.locator("#edit-credential").selectOption("key");
  await page.locator("#edit-key-file").setInputFiles(privateKeyPath);
  await page.getByRole("button", { name: "保存连接信息" }).click();
  await page.getByRole("heading", { name: "connection-target", exact: true }).waitFor();
}

async function startInlineConnectionTest(page: import("@playwright/test").Page): Promise<void> {
  const detailURL = page.url();
  await page.getByRole("button", { name: "测试连接" }).click();
  await expect(page).toHaveURL(detailURL);
  await expect(page.getByText("正在连接…")).toBeVisible({ timeout: 5_000 });
}

async function assertCandidateToggle(page: import("@playwright/test").Page): Promise<void> {
  const toggle = page.locator("[data-connection-test-candidates]");
  await expect(page.getByRole("button", { name: "展开看每个候选地址的结果" })).toBeVisible();
  const candidates = page.locator("#server-connection-test-candidates");
  await expect(toggle).toHaveAttribute("aria-expanded", "false");
  await expect(candidates).toBeHidden();
  await toggle.click();
  await expect(toggle).toHaveAttribute("aria-expanded", "true");
  await expect(candidates).toBeVisible();
  await expect(candidates.locator(".connection-test-candidate").first()).toContainText("127.0.0.1");
  await toggle.click();
  await expect(toggle).toHaveAttribute("aria-expanded", "false");
  await expect(candidates).toBeHidden();
}

test("服务器详情连接测试成功后原位显示并跨重启保存", async ({ page }) => {
  const keyDirectory = "/tmp/sshmgr-connection-test-key";
  await mkdir(keyDirectory, { recursive: true });
  const sshd = await startTestSSHServer(path.join(keyDirectory, "success-key.pem"), {
    handshakeDelayMilliseconds: 400,
  });
  const [, port] = sshd.address.split(":");
  const ui = await startStandaloneUIWithTopology(undefined, Number(port));
  try {
    await openConnectionTarget(page, ui);
    await usePrivateKey(page, "127.0.0.1", port, sshd.privateKeyPath);
    await startInlineConnectionTest(page);

    const success = page.locator(".connection-test-result.is-succeeded");
    await expect(success).toContainText("✓");
    await expect(success).toContainText("已连上 · 用的地址 127.0.0.1（连接测试局域网）· 耗时", {
      timeout: 15_000,
    });
    await expect(success).toContainText(/耗时 \d+ 毫秒/);
    await expect(page.locator("#server-last-test")).toContainText(/上次测试：.+ 已连上/);
    await expect(page.getByText("服务器列表卡片上的显示")).toHaveCount(0);

    const restartedURL = await ui.restart();
    await page.goto(restartedURL);
    await page.getByRole("heading", { name: "状态总览" }).waitFor();
    await gotoUIPath(page, connectionTargetPath);
    await expect(page.locator("#server-last-test")).toContainText(/上次测试：.+ 已连上/);
    await expect(page.locator(".connection-test-result.is-succeeded")).toContainText(
      "已连上 · 用的地址 127.0.0.1（连接测试局域网）",
    );
  } finally {
    await ui.close();
    await sshd.close();
  }
});

test("服务器详情连接测试认证失败并展示候选地址", async ({ page }) => {
  const keyDirectory = "/tmp/sshmgr-connection-test-auth-key";
  await mkdir(keyDirectory, { recursive: true });
  const sshd = await startTestSSHServer(path.join(keyDirectory, "auth-key.pem"), {
    handshakeDelayMilliseconds: 400,
  });
  const [, port] = sshd.address.split(":");
  const ui = await startStandaloneUIWithTopology(undefined, Number(port));
  try {
    await openConnectionTarget(page, ui);
    await startInlineConnectionTest(page);
    const failure = page.locator(".connection-test-result.is-failed");
    await expect(failure).toContainText("✗");
    await expect(failure).toContainText("连不上 · 认证失败", { timeout: 15_000 });
    await assertCandidateToggle(page);
    await expect(page.locator("body")).not.toContainText("browser-test-password");
    await expect(page.locator("body")).not.toContainText("unable to authenticate");
  } finally {
    await ui.close();
    await sshd.close();
  }
});

test("服务器详情连接测试端口不通并展示候选地址", async ({ page }) => {
  const port = await reserveFreePort();
  const ui = await startStandaloneUIWithTopology(undefined, port);
  try {
    await openConnectionTarget(page, ui);
    await page.getByRole("navigation", { name: "服务器详情分段" }).getByRole("link", { name: "地址与地点" }).click();
    await page
      .locator(".address-card-row", { hasText: "连接测试备用局域网" })
      .getByRole("button", { name: "移除" })
      .click();
    await expect(page.getByText("已移除地址。")).toBeVisible();
    await page.getByRole("navigation", { name: "服务器详情分段" }).getByRole("link", { name: "连接" }).click();
    await startInlineConnectionTest(page);
    const failure = page.locator(".connection-test-result.is-failed");
    await expect(failure).toContainText("✗");
    await expect(failure).toContainText("连不上 · 端口不通", { timeout: 15_000 });
    await assertCandidateToggle(page);
  } finally {
    await ui.close();
  }
});

test("服务器详情连接测试超时并展示候选地址", async ({ page }) => {
  const silentServer = await startSilentSSHServer();
  const port = Number(silentServer.address.split(":")[1]);
  const ui = await startStandaloneUIWithTopology(undefined, port, {
    SSHMGR_SSH_ADDRESS_TIMEOUT: "300ms",
  });
  try {
    await openConnectionTarget(page, ui);
    await startInlineConnectionTest(page);
    const failure = page.locator(".connection-test-result.is-failed");
    await expect(failure).toContainText("✗");
    await expect(failure).toContainText("连不上 · 超时", { timeout: 15_000 });
    await assertCandidateToggle(page);
  } finally {
    await ui.close();
    await silentServer.close();
  }
});
