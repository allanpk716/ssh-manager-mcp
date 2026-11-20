import { mkdir } from "node:fs/promises";
import { copyFileSync, mkdirSync, writeFileSync } from "node:fs";
import path from "node:path";
import {
  expect,
  gotoUIPath,
  openStandaloneUI,
  startStandaloneUI,
  startStandaloneUIWithTopology,
  startTestSSHServer,
  test,
} from "../helpers";

// serverCard 定位某一台服务器所在的卡片，再在卡片内找链接，避免同名链接
// 互相干扰。
function serverCard(page: import("@playwright/test").Page, name: string) {
  return page.locator("[data-server-card]", { has: page.getByRole("link", { name, exact: true }) });
}

async function filterAttentionOnly(page: import("@playwright/test").Page) {
  await page.getByLabel("只看需要注意").check();
  await page.waitForURL(/attention=1/);
}

// connectionTargetCard 定位连接测试专用条目所在的列表卡片。
function connectionTargetCard(page: import("@playwright/test").Page) {
  return serverCard(page, "connection-target");
}

// usePrivateKey 把连接测试专用条目改成私钥登录，指向本次启动的 SSH 测试
// 服务。
async function usePrivateKey(
  page: import("@playwright/test").Page,
  port: string,
  privateKeyPath: string,
): Promise<void> {
  await page.getByRole("region", { name: "连接" }).getByRole("link", { name: "编辑" }).click();
  await page.locator("#edit-port").fill(port);
  await page.locator("#edit-credential").selectOption("key");
  await page.locator("#edit-key-file").setInputFiles(privateKeyPath);
  await page.getByRole("button", { name: "保存连接信息" }).click();
  await page.getByRole("heading", { name: "connection-target", exact: true }).waitFor();
}

// assertListStatusFitsMobileViewport 断言手机宽度下列表没有横向溢出。
async function assertListStatusFitsMobileViewport(page: import("@playwright/test").Page) {
  const overflow = await page.evaluate(
    () => document.documentElement.scrollWidth - document.documentElement.clientWidth,
  );
  expect(overflow).toBeLessThanOrEqual(0);
}

test("过滤需要注意的服务器并编辑补齐角色", async ({ page, standaloneUI }) => {
  await openStandaloneUI(page, standaloneUI);

  await filterAttentionOnly(page);
  await expect(serverCard(page, "db-1")).toContainText("未设角色");
  await expect(serverCard(page, "web-1")).toHaveCount(0);

  await serverCard(page, "db-1").getByRole("link", { name: "编辑" }).click();
  await expect(page.getByRole("heading", { name: "db-1" })).toBeVisible();
  await page.locator('[data-server-detail-drawer="connection"]').getByRole("link", { name: "取消" }).click();
  await page.locator('[data-detail-edit-button="description"]').click();
  await page.getByLabel("用途", { exact: true }).fill("生产数据库");
  await page.getByRole("button", { name: "保存说明" }).click();

  await expect(page.getByRole("heading", { name: "db-1" })).toBeVisible();
  await expect(page.getByText("已保存说明。")).toBeVisible();
  await expect(page.locator("#server-card-description")).toContainText("生产数据库");

  await page.getByRole("link", { name: "返回服务器列表" }).click();
  await filterAttentionOnly(page);
  await expect(serverCard(page, "db-1")).toHaveCount(0);
});

test("列表卡片显示最近一次连接测试结果并跨重启保留", async ({ page }) => {
  const keyDirectory = "/tmp/sshmgr-servers-list-connection-key";
  await mkdir(keyDirectory, { recursive: true });
  const sshd = await startTestSSHServer(path.join(keyDirectory, "success-key.pem"), {
    handshakeDelayMilliseconds: 400,
  });
  const [, port] = sshd.address.split(":");
  const ui = await startStandaloneUIWithTopology(undefined, Number(port));
  try {
    await page.setViewportSize({ width: 390, height: 844 });
    await page.goto(ui.startupURL);
    await page.getByRole("heading", { name: "状态总览" }).waitFor();
    await gotoUIPath(page, "/ui/servers/");

    const card = connectionTargetCard(page);
    await expect(card.locator(".server-test-line.is-untested")).toContainText("从未测试");
    await expect(page.locator(".server-test-line.is-untested").first()).toContainText("从未测试");
    await assertListStatusFitsMobileViewport(page);

    await card.getByRole("link", { name: "详情" }).click();
    await page.getByRole("heading", { name: "connection-target", exact: true }).waitFor();
    await usePrivateKey(page, port, sshd.privateKeyPath);
    await page.getByRole("button", { name: "测试连接" }).click();
    await expect(page.locator(".connection-test-result.is-succeeded")).toContainText(
      "已连上 · 用的地址 127.0.0.1（连接测试局域网）",
      { timeout: 15_000 },
    );

    await page.getByRole("link", { name: "返回服务器列表" }).click();
    await page.getByRole("heading", { name: /^服务器/ }).first().waitFor();
    const success = card.locator(".server-test-line.is-succeeded");
    await expect(success).toContainText(/已连上/);
    await expect(success).toContainText("127.0.0.1");
    await expect(success).toContainText("连接测试局域网");
    await expect(success).toContainText(/耗时 \d+ 毫秒/);
    await expect(card.locator(".server-test-line.is-untested")).toHaveCount(0);
    await expect(page.locator(".connection-test-candidate-list")).toHaveCount(0);
    await expect(page.getByRole("button", { name: "批量测试连接" })).toHaveCount(0);
    await assertListStatusFitsMobileViewport(page);

    const restartedURL = await ui.restart();
    await page.goto(restartedURL);
    await page.getByRole("heading", { name: "状态总览" }).waitFor();
    await gotoUIPath(page, "/ui/servers/");
    await expect(connectionTargetCard(page).locator(".server-test-line.is-succeeded")).toContainText(
      "已连上",
    );
    await expect(
      connectionTargetCard(page).locator(".server-test-line.is-succeeded"),
    ).toContainText("127.0.0.1");
  } finally {
    await ui.close();
    await sshd.close();
  }
});

test("服务器筛选选完立即生效并可用胶囊清除", async ({ page }) => {
  const ui = await startStandaloneUIWithTopology(undefined, undefined, {
    SSHMGR_E2E_SERVER_FILTERS: "1",
  });
  try {
    await page.goto(ui.startupURL);
    await page.getByRole("heading", { name: "状态总览" }).waitFor();
    await gotoUIPath(page, "/ui/servers/");
    await expect(page.getByRole("button", { name: "应用筛选" })).toBeHidden();

    await page.getByRole("button", { name: /全部地点/ }).click();
    await page.getByRole("heading", { name: "按地点筛选" }).waitFor();
    const companyOption = page.getByRole("button", { name: "公司 包含全部下级地点 3 台" });
    await companyOption.click();
    await page.waitForURL((url) => url.searchParams.has("site"));
    await expect(serverCard(page, "company-root")).toBeVisible();
    await expect(serverCard(page, "factory-web")).toBeVisible();
    await expect(serverCard(page, "building-db")).toBeVisible();
    await expect(serverCard(page, "home-machine")).toHaveCount(0);

    await page.getByRole("link", { name: "清除地点：公司" }).click();
    await page.waitForURL((url) => !url.searchParams.has("site"));

    await page.getByRole("button", { name: /全部网络/ }).click();
    await page.getByRole("heading", { name: "按网络筛选" }).waitFor();
    await page.getByRole("button", { name: /公司办公局域网/ }).click();
    await page.waitForURL((url) => url.searchParams.has("network"));
    await expect(serverCard(page, "company-root")).toBeVisible();
    await expect(serverCard(page, "factory-web")).toBeVisible();
    await expect(serverCard(page, "building-db")).toHaveCount(0);

    await page.getByRole("link", { name: "清除网络：公司办公局域网" }).click();
    await page.getByRole("button", { name: /全部地点/ }).click();
    const unregisteredOption = page.getByRole("button", { name: /未登记地点/ });
    await unregisteredOption.focus();
    await page.keyboard.press("Enter");
    await page.waitForURL((url) => url.searchParams.get("site") === "__unregistered__");
    await expect(serverCard(page, "missing-site")).toBeVisible();
    await expect(serverCard(page, "no-device")).toBeVisible();
    await expect(serverCard(page, "company-root")).toHaveCount(0);

    await page.getByRole("link", { name: "清除全部" }).click();
    await page.waitForURL((url) => url.search === "");
    await page.getByLabel("搜索服务器").fill("factory");
    await page.waitForURL(/query=factory/);
    await expect(serverCard(page, "factory-web")).toBeVisible();
    await expect(serverCard(page, "company-root")).toHaveCount(0);
  } finally {
    await ui.close();
  }
});

test("删除前查看影响预览并一次确认", async ({ page, standaloneUI }) => {
  await openStandaloneUI(page, standaloneUI);

  await serverCard(page, "old-1").getByRole("link", { name: "删除" }).click();
  await expect(page.getByRole("heading", { name: "删除 old-1" })).toBeVisible();
  await expect(page.getByText("设备保留：old-1")).toBeVisible();
  await expect(page.getByText("主机密钥锚")).toBeVisible();
  await expect(page.getByText("失去这台服务器的 profile").locator("..")).toContainText("运维");

  await page.getByRole("button", { name: "确认删除" }).click();
  await expect(page.getByRole("heading", { name: "服务器" })).toBeVisible();
  await expect(serverCard(page, "old-1")).toHaveCount(0);
  await expect(serverCard(page, "db-1")).toBeVisible();
});

// importFixture 在 Web UI 进程所在机器的临时目录里写入 SSH 配置与加密私钥，
// 返回配置文件路径。
function importFixture(runDir: string): string {
  const sshDir = path.join(runDir, ".ssh");
  mkdirSync(sshDir, { recursive: true });
  const keyPath = path.join(sshDir, "encrypted_key");
  copyFileSync(path.join(__dirname, "fixtures", "encrypted_ec.pem"), keyPath);
  const configPath = path.join(sshDir, "config");
  writeFileSync(
    configPath,
    `Host e2e-enc
  HostName 192.0.2.30
  User deploy
  IdentityFile ${keyPath}

Host e2e-bare
  HostName 192.0.2.31
  User deploy
`,
    "utf8",
  );
  return configPath;
}

test("导入 SSH 配置后逐台补全（含跳过与结束）", async ({ page }) => {
  let configPath = "";
  const ui = await startStandaloneUI((runDir) => {
    configPath = importFixture(runDir);
  });
  try {
    await openStandaloneUI(page, ui);

    await page.getByRole("link", { name: "导入 SSH 配置" }).click();
    await page.getByLabel("SSH 配置路径").fill(configPath);
    await page.getByRole("button", { name: "查看候选" }).click();

    await expect(page.getByText("e2e-enc — deploy@192.0.2.30:22")).toBeVisible();
    await expect(page.getByText("待补私钥口令")).toBeVisible();
    await expect(page.getByText("e2e-bare — deploy@192.0.2.31:22")).toBeVisible();
    await page.getByRole("button", { name: "执行导入" }).click();

    await expect(page.getByText("导入 2 · 跳过 0 · 失败 0 · 待补 2")).toBeVisible();
    await page.getByRole("link", { name: "开始逐台补全" }).click();

    // 第一台：跳过，保留警告。
    await expect(page.getByRole("heading", { name: /补全 e2e-enc/ })).toBeVisible();
    await expect(page.getByLabel("私钥口令（补全加密私钥）")).toBeVisible();
    await page.getByRole("button", { name: "跳过这一台" }).click();

    // 第二台：填写角色后结束整个补全；「待补」按当前数据重新计算。
    await expect(page.getByRole("heading", { name: /补全 e2e-bare/ })).toBeVisible();
    await page.getByLabel("角色", { exact: true }).fill("计算节点");
    await page.getByRole("button", { name: "结束补全" }).click();

    await expect(page.getByRole("heading", { name: "补全结果" })).toBeVisible();
    await expect(page.getByText("导入 2 · 跳过 0 · 失败 0 · 待补 2")).toBeVisible();
  } finally {
    await ui.close();
  }
});
