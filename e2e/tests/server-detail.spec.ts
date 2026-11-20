import { mkdir } from "node:fs/promises";
import path from "node:path";
import {
  expect,
  gotoUIPath,
  reserveFreePort,
  startClientUIWithTopology,
  startSilentSSHServer,
  startStandaloneUIWithTopology,
  startTestSSHServer,
  test,
} from "../helpers";

async function openDetail(
  page: import("@playwright/test").Page,
  ui: { startupURL: string },
): Promise<void> {
  await page.goto(ui.startupURL);
  await page.getByRole("heading", { name: "状态总览" }).waitFor();
  await gotoUIPath(page, "/ui/servers/web-1");
  await page.getByRole("heading", { name: "web-1", exact: true }).waitFor();
}

test("服务器详情一列卡片并回归旧分段地址", async ({ page }) => {
  const ui = await startStandaloneUIWithTopology();
  try {
    await openDetail(page, ui);
    await expect(page.getByRole("navigation", { name: "服务器详情分段" })).toHaveCount(0);

    const cardIDs = await page.locator(".detail-section > section").evaluateAll((sections) =>
      sections.map((section) => section.id),
    );
    const expectedOrder = [
      "server-card-connection",
      "server-card-location",
      "server-card-address",
      "server-card-description",
      "server-card-accounts",
      "server-card-authorization",
      "server-card-software",
    ];
    let position = -1;
    for (const id of expectedOrder) {
      const current = cardIDs.indexOf(id);
      expect(current, `卡片 ${id} 应存在`).toBeGreaterThan(-1);
      expect(current, `卡片 ${id} 应按顺序出现`).toBeGreaterThan(position);
      position = current;
      await expect(page.locator(`#${id}`)).toBeVisible();
    }
    await expect(page.locator("#server-advanced-actions")).not.toHaveAttribute("open", "");

    // 同机账号卡片：账号、说明与最近测试状态都来自持久数据。
    const accounts = page.locator("#server-card-accounts");
    await expect(accounts.getByRole("link", { name: "db-1" })).toBeVisible();
    await expect(accounts).toContainText("每晚备份任务使用");
    await expect(accounts).toContainText(/上次测试：.+ 已连上/);

    // 软件清单卡片沿用设备软件清单页的摘要，并进入设备详情。
    const software = page.locator("#server-card-software");
    for (const forbidden of ["声明", "认领", "草稿", "待办", "待确认暴露"]) {
      await expect(software).not.toContainText(forbidden);
    }
    await expect(software).toContainText("采集时间");
    await expect(software).toContainText("正常");
    const softwareEntry = software.getByRole("link", { name: "查看软件清单" });
    await expect(softwareEntry).toHaveAttribute(
      "href",
      /\/ui\/software-inventory\/devices\?device=%E5%B7%A5%E4%BD%9C%E7%AB%99$/,
    );
    await softwareEntry.click();
    await page.getByRole("heading", { name: "工作站 · 软件清单" }).waitFor();
    await expect(page).toHaveURL(/\/ui\/software-inventory\/devices\?device=/);
    await page.goBack();
    await page.getByRole("heading", { name: "web-1", exact: true }).waitFor();
    await expect(page.locator("#server-card-software")).toBeVisible();

    // 旧分段查询地址回到统一详情页。
    for (const tab of ["connection", "addresses", "authorization", "software"]) {
      await gotoUIPath(page, `/ui/servers/web-1?tab=${tab}`);
      await page.getByRole("heading", { name: "web-1", exact: true }).waitFor();
      await expect(page).toHaveURL(/\/ui\/servers\/web-1$/);
      await expect(page.locator("#server-card-software")).toBeVisible();
    }
  } finally {
    await ui.close();
  }
});

test("服务器详情分区编辑弹层支持打开取消保存与焦点返回", async ({ page }) => {
  const ui = await startStandaloneUIWithTopology();
  try {
    await openDetail(page, ui);
    const drawers = [
      { scope: "connection", title: "编辑连接" },
      { scope: "location", title: "编辑地点" },
      { scope: "description", title: "编辑说明" },
      { scope: "accounts", title: "编辑其他账号说明" },
    ];
    for (const drawer of drawers) {
      const editButton = page.locator(`[data-detail-edit-button="${drawer.scope}"]`);
      await editButton.click();
      const dialog = page.locator(`[data-server-detail-drawer="${drawer.scope}"]`);
      await expect(dialog).toBeVisible();
      await expect(dialog.getByRole("heading", { name: drawer.title })).toBeVisible();
      await expect(
        dialog.locator("input:visible, select:visible, textarea:visible").first(),
      ).toBeFocused();
      await expect(page.locator(`[data-server-detail-drawer="${drawer.scope}"]`)).toHaveCount(1);
      await dialog.getByRole("link", { name: "取消" }).click();
      await page.getByRole("heading", { name: "web-1", exact: true }).waitFor();
      await expect(page.locator(`[data-server-detail-drawer="${drawer.scope}"]`)).toHaveCount(0);
      await expect(editButton).toBeFocused();
    }

    // 保存地点：回到地点卡片，焦点回到编辑按钮。
    await page.locator('[data-detail-edit-button="location"]').click();
    await page.getByRole("radio", { name: "机房", exact: true }).check();
    await page.getByRole("button", { name: "保存地点" }).click();
    await expect(page.getByText("已保存机器地点。")).toBeVisible();
    await expect(page.locator(".location-value")).toHaveText("机房");
    await expect(page.locator('[data-detail-edit-button="location"]')).toBeFocused();

    // 保存说明：卡片逐行更新。
    await page.locator('[data-detail-edit-button="description"]').click();
    await page.locator("#edit-description").fill("浏览器测试备注");
    await page.getByRole("button", { name: "保存说明" }).click();
    await expect(page.getByText("已保存说明。")).toBeVisible();
    await expect(page.locator("#server-card-description")).toContainText("浏览器测试备注");
    await expect(page.locator('[data-detail-edit-button="description"]')).toBeFocused();

    // 保存同机账号说明：只改显示说明，登录信息仍在各自详情页。
    await page.locator('[data-detail-edit-button="accounts"]').click();
    await page.getByLabel("db-1（owner）的显示说明").fill("夜里跑备份任务");
    await expect(
      page
        .locator('[data-server-detail-drawer="accounts"]')
        .getByRole("link", { name: "db-1 的详情页" }),
    ).toHaveAttribute("href", /\/ui\/servers\//);
    await page.getByRole("button", { name: "保存账号说明" }).click();
    await expect(page.getByText("已保存账号说明。")).toBeVisible();
    await expect(page.locator("#server-card-accounts")).toContainText("夜里跑备份任务");
    await expect(page.locator('[data-detail-edit-button="accounts"]')).toBeFocused();
  } finally {
    await ui.close();
  }
});

test("客户端服务器详情只读并提示到权威端修改", async ({ page }) => {
  const ui = await startClientUIWithTopology();
  try {
    await page.goto(ui.startupURL);
    await page.getByRole("heading", { name: "状态总览" }).waitFor();
    await gotoUIPath(page, "/ui/servers/client-server");
    await page.getByRole("heading", { name: "客户端服务器", exact: true }).waitFor();
    await expect(page.locator("[data-detail-edit-button]")).toHaveCount(0);
    await expect(page.locator("#server-card-connection")).toContainText(
      "仅显示本地快照，修改请到权威端。",
    );
    await expect(page.locator("#server-card-authorization")).toContainText(
      "仅显示本客户端可用的部分，修改请到权威端。",
    );
    await expect(page.locator("#server-card-software")).toBeVisible();
  } finally {
    await ui.close();
  }
});

test("手机高级操作改挂需要二次确认并显示结果", async ({ page, isMobile }, testInfo) => {
  testInfo.skip(!isMobile, "这个用例按 390 视口验证手机流程");
  const ui = await startStandaloneUIWithTopology();
  try {
    await openDetail(page, ui);
    const advanced = page.locator("#server-advanced-actions");
    await expect(advanced).not.toHaveAttribute("open", "");
    await advanced.locator("summary").click();
    await expect(advanced).toContainText(
      "这里只放低频且有风险的操作。改挂会改变这个条目关联的设备。",
    );
    await expect(advanced.getByRole("link", { name: "改挂到另一台设备" })).toBeVisible();
    await expect(advanced.getByRole("link", { name: "删除服务器条目" })).toBeVisible();
    await expect(
      page.locator("#server-card-address").getByRole("button", { name: /改挂/ }),
    ).toHaveCount(0);

    const entry = advanced.getByRole("link", { name: "改挂到另一台设备" });
    await entry.click();
    const dialog = page.locator("[data-server-move-dialog]");
    await expect(dialog).toBeVisible();
    await expect(dialog).toContainText(
      "改挂后这个条目将使用目标机器的地点与地址；登录信息、授权、其他地址不变；软件清单留在原机器。",
    );
    await dialog.getByLabel("目标设备").selectOption("NAS");
    const confirm = dialog.getByRole("button", { name: "确认改挂" });
    await expect(confirm).toBeDisabled();

    await dialog.getByRole("link", { name: "取消" }).click();
    await page.getByRole("heading", { name: "web-1", exact: true }).waitFor();
    await expect(page.locator("[data-server-move-dialog]")).toHaveCount(0);
    await expect(entry).toBeFocused();

    await entry.click();
    await expect(dialog).toBeVisible();
    await dialog.getByLabel("目标设备").selectOption("NAS");
    await dialog.getByLabel("我已了解").check();
    await expect(confirm).toBeEnabled();
    await confirm.click();

    await expect(
      page.getByText("已改挂到目标设备 NAS。无可复制的主机密钥指纹。"),
    ).toBeVisible();
    await expect(page.locator(".location-value")).toHaveText("未登记地点");
    const addresses = page.locator("#server-card-address");
    await expect(addresses).toContainText("192.168.1.30");
    await expect(addresses).toContainText("100.64.0.30");
    await expect(page.locator("[data-server-move-dialog]")).toHaveCount(0);
  } finally {
    await ui.close();
  }
});

const connectionTargetPath = "/ui/servers/connection-target";

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

    const retestButton = page.getByRole("button", { name: "测试连接", exact: true });
    await expect(retestButton).toBeVisible();
    await expect(retestButton).toBeEnabled();
    await startInlineConnectionTest(page);
    await expect(
      page.locator(".connection-test-result.is-succeeded"),
    ).toContainText("已连上 · 用的地址 127.0.0.1（连接测试局域网）", { timeout: 15_000 });

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
    await page
      .locator(".address-card-row", { hasText: "连接测试备用局域网" })
      .getByRole("button", { name: "移除" })
      .click();
    await expect(page.getByText("已移除地址。")).toBeVisible();
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
