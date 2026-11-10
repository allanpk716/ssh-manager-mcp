import { expect, startEmptyMachineUI, test } from "../helpers";

test("空机器走完单机初始化向导并延续会话进入正式首页", async ({ page }) => {
  const ui = await startEmptyMachineUI();
  try {
    await page.goto(ui.startupURL);

    await expect(page.getByRole("heading", { name: "选择本机角色" })).toBeVisible();
    await expect(page.getByText("一台机器自持 vault，适合只在当前电脑上使用 sshmgr。")).toBeVisible();
    await expect(page.getByText("多机部署里常驻权威 vault，其余机器经配对入网。")).toBeVisible();
    await expect(page.getByText("只持本地只读快照，修改数据到权威端 Web UI 做。")).toBeVisible();

    await page.getByRole("button", { name: "选择单机" }).click();
    await expect(page.getByRole("heading", { name: "初始化单机" })).toBeVisible();

    await page.getByRole("button", { name: "创建 vault" }).click();
    await expect(page.getByRole("heading", { name: "vault 已创建" })).toBeVisible();
    await expect(page.getByText(/master\.key\.plain/)).toBeVisible();
    await expect(page.getByText("请备份此文件。")).toBeVisible();

    await page.getByRole("button", { name: "进入单机正式页面" }).click();
    await expect(page.getByRole("heading", { name: "服务器" })).toBeVisible();
    await expect(page).toHaveURL(/\/ui\/$/);

    const nextSteps = page.locator(".next-steps");
    await expect(nextSteps.getByRole("heading", { name: "下一步引导" })).toBeVisible();
    await expect(nextSteps.getByRole("link", { name: "录服务器" })).toBeVisible();
    await expect(nextSteps.getByRole("link", { name: "建 Profile 并授权" })).toBeVisible();
    await expect(nextSteps.getByRole("link", { name: "建项目并取令牌" })).toBeVisible();
  } finally {
    await ui.close();
  }
});
