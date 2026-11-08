import { copyFileSync, mkdirSync, writeFileSync } from "node:fs";
import path from "node:path";
import { expect, openStandaloneUI, startStandaloneUI, test } from "../helpers";

// serverRow 定位某一台服务器所在的表格行，再在行内找链接，避免同名链接互
// 相干扰。
function serverRow(page: import("@playwright/test").Page, name: string) {
  return page.locator("tr", { has: page.getByRole("link", { name, exact: true }) });
}

test("过滤需要注意的服务器并编辑补齐角色", async ({ page, standaloneUI }) => {
  await openStandaloneUI(page, standaloneUI);

  await page.getByRole("link", { name: "只看需要注意" }).click();
  await expect(serverRow(page, "db-1")).toContainText("未设角色");
  await expect(serverRow(page, "web-1")).toHaveCount(0);

  await serverRow(page, "db-1").getByRole("link", { name: "编辑" }).click();
  await page.getByLabel("角色", { exact: true }).fill("生产数据库");
  await page.getByRole("button", { name: "保存" }).click();

  await expect(page.getByRole("heading", { name: "db-1" })).toBeVisible();
  await expect(page.getByText("生产数据库")).toBeVisible();

  await page.getByRole("link", { name: "返回服务器列表" }).click();
  await page.getByRole("link", { name: "只看需要注意" }).click();
  await expect(serverRow(page, "db-1")).toHaveCount(0);
});

test("删除前查看影响预览并一次确认", async ({ page, standaloneUI }) => {
  await openStandaloneUI(page, standaloneUI);

  await serverRow(page, "old-1").getByRole("link", { name: "删除" }).click();
  await expect(page.getByRole("heading", { name: "删除 old-1" })).toBeVisible();
  await expect(page.getByText("设备保留：old-1")).toBeVisible();
  await expect(page.getByText("主机密钥锚")).toBeVisible();
  await expect(page.getByText("失去这台服务器的 profile").locator("..")).toContainText("运维");

  await page.getByRole("button", { name: "确认删除" }).click();
  await expect(page.getByRole("heading", { name: "服务器" })).toBeVisible();
  await expect(serverRow(page, "old-1")).toHaveCount(0);
  await expect(serverRow(page, "db-1")).toBeVisible();
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
