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

    // 软件清单卡片只保留设备视图过渡入口。
    const software = page.locator("#server-card-software");
    for (const forbidden of ["声明", "认领", "草稿", "待办", "待确认暴露"]) {
      await expect(software).not.toContainText(forbidden);
    }
    await expect(software.getByRole("link", { name: "查看软件清单" })).toHaveAttribute(
      "href",
      /\/ui\/software-inventory\/devices\?device=/,
    );

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
      { scope: "address", title: "编辑地址" },
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

test("服务器详情地址统一列表支持改归网络与删除行", async ({ page }) => {
  const ui = await startStandaloneUIWithTopology();
  try {
    await openDetail(page, ui);
    const card = page.locator("#server-card-address");
    for (const row of [
      { network: "家A局域网", type: "物理局域网", address: "192.168.1.20" },
      { network: "虚拟网", type: "虚拟局域网", address: "100.64.0.20" },
      { network: "虚拟网B", type: "虚拟局域网", address: "100.65.0.20" },
      { network: "其他（公网或未登记网络）", type: "其他地址", address: "192.0.2.10" },
    ]) {
      const addressRow = card.locator(".address-card-row", { hasText: row.address }).filter({
        hasText: row.network,
      });
      await expect(addressRow).toBeVisible();
      await expect(addressRow).toContainText(row.type);
    }

    await page.locator('[data-detail-edit-button="address"]').click();
    const drawer = page.locator('[data-server-detail-drawer="address"]');
    await expect(drawer).toBeVisible();
    const otherRow = drawer.locator('[data-address-row]:has(input[value="192.0.2.10"])');
    await expect(otherRow.locator('[data-address-network]')).toHaveValue("other");
    await expect(
      drawer.locator('[data-address-row]:has(input[value="192.168.1.20"]) option[value="other"]'),
    ).toBeDisabled();
    await otherRow.locator('[data-address-network]').selectOption("机房局域网");
    await drawer.getByRole("button", { name: "保存地址" }).click();

    await expect(page.getByText("已保存地址。")).toBeVisible();
    const reassigned = card.locator(".address-card-row", { hasText: "192.0.2.10" });
    await expect(reassigned).toContainText("机房局域网");
    await expect(reassigned).toContainText("物理局域网");
    await expect(card).not.toContainText("其他（公网或未登记网络）");
    await expect(page.locator('[data-detail-edit-button="address"]')).toBeFocused();

    await page.locator('[data-detail-edit-button="address"]').click();
    const deleteRow = drawer.locator('[data-address-row]:has(input[value="192.0.2.10"])');
    await deleteRow.getByRole("button", { name: "删除第4行地址" }).click();
    await drawer.getByRole("button", { name: "保存地址" }).click();
    await expect(page.getByText("已保存地址。")).toBeVisible();
    await expect(card).not.toContainText("192.0.2.10");
    await expect(card).toContainText("192.168.1.20");
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
    await page.locator('[data-detail-edit-button="address"]').click();
    const addressDrawer = page.locator('[data-server-detail-drawer="address"]');
    await addressDrawer
      .locator('[data-address-row]:has(input[value="127.0.0.2"])')
      .getByRole("button", { name: /删除第\d行地址/ })
      .click();
    await addressDrawer.getByRole("button", { name: "保存地址" }).click();
    await expect(page.getByText("已保存地址。")).toBeVisible();
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
