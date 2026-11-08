import { expect, openStandaloneUI, test } from "../helpers";

test("调整 profile 授权后显示结果摘要", async ({ page, standaloneUI }) => {
  await openStandaloneUI(page, standaloneUI);

  await page.getByRole("link", { name: "访问" }).click();
  await page.getByRole("link", { name: "Profile" }).click();
  await page.getByRole("link", { name: "调整授权" }).click();
  await page.getByRole("checkbox", { name: "db-1" }).check();
  await page.getByRole("checkbox", { name: "web-1" }).uncheck();
  await page.getByRole("button", { name: "保存授权" }).click();

  await expect(
    page.getByText("已保存 运维 的授权：新增授权：db-1；撤销授权：web-1"),
  ).toBeVisible();
});

test("签发项目令牌只在提交响应里显示一次", async ({ page, standaloneUI }) => {
  await openStandaloneUI(page, standaloneUI);

  await page.getByRole("link", { name: "项目令牌" }).click();
  await page.getByLabel("签发新项目令牌").fill("e2e-agent");
  await page.getByLabel("绑定 profile").selectOption({ label: "运维" });
  await page.getByRole("button", { name: "签发", exact: true }).click();

  const token = (await page.locator("#project-token-value").innerText()).trim();
  expect(token).toMatch(/^[A-Za-z0-9_-]{40,}$/);
  const snippet = (await page.locator("#project-snippet-value").innerText()).trim();
  expect(snippet).toBe(
    `{"mcpServers":{"ssh":{"command":"sshmgr","args":["mcp"],"env":{"SSHMGR_TOKEN":"${token}"}}}}`,
  );
  await expect(page.getByText("不要把含令牌的文件提交进 git")).toBeVisible();
  await expect(page.getByText("离开本页后不能再查看")).toBeVisible();

  await page.reload();
  await expect(page.getByRole("heading", { name: "项目令牌" })).toBeVisible();
  await expect(page.locator("#project-token-value")).toHaveCount(0);
  expect(await page.content()).not.toContain(token);
});
