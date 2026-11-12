import { expect, openStandaloneUI, startStandaloneUIWithTopology, test } from "../helpers";

test("桌面从类型选择页新增有多个网络成员的其他设备", async ({ page, isMobile }, testInfo) => {
  testInfo.skip(isMobile, "这个用例只运行桌面视口");
  const ui = await startStandaloneUIWithTopology();
  try {
    await openStandaloneUI(page, ui);

    await page.getByRole("link", { name: "添加", exact: true }).click();
    await page.getByRole("link", { name: /其他设备/ }).click();
    await expect(page.getByRole("heading", { name: "新增其他设备" })).toBeVisible();
    await expect(page.getByLabel("登录凭据")).toHaveCount(0);
    await expect(page.getByText("profile 授权")).toHaveCount(0);
    await expect(page.getByText("连接测试")).toHaveCount(0);

    await page.getByLabel("设备名", { exact: true }).fill("桌面网络存储");
    await page.getByLabel("备注", { exact: true }).fill("共享照片与备份");
    await page.getByLabel("站点", { exact: true }).selectOption("家A");
    await page.getByLabel("物理局域网", { exact: true }).selectOption("家A局域网");
    await page.getByLabel("物理局域网地址").fill("192.168.1.80");
    await page.getByLabel("虚拟局域网", { exact: true }).first().selectOption("虚拟网");
    await page.getByLabel("地址", { exact: true }).first().fill("100.64.0.80");
    await page.getByRole("button", { name: "保存设备" }).click();

    await expect(page.getByRole("heading", { name: "已保存其他设备" })).toBeVisible();
    await expect(page.getByText("桌面网络存储")).toBeVisible();
    await expect(page.getByText("不进入服务器连接测试队列")).toBeVisible();
  } finally {
    await ui.close();
  }
});

test("手机新增暂不归类的其他设备", async ({ page, isMobile }, testInfo) => {
  testInfo.skip(!isMobile, "这个用例只运行手机视口");
  const ui = await startStandaloneUIWithTopology();
  try {
    await openStandaloneUI(page, ui);
    await page.getByRole("link", { name: "添加", exact: true }).click();
    await page.getByRole("link", { name: /其他设备/ }).click();

    await page.getByLabel("设备名", { exact: true }).fill("手机未归类设备");
    await page.getByLabel("备注", { exact: true }).fill("稍后补充位置");
    await page.getByRole("button", { name: "保存设备" }).click();

    await expect(page.getByRole("heading", { name: "已保存其他设备" })).toBeVisible();
    await expect(page.getByText("手机未归类设备")).toBeVisible();
    const overflow = await page.evaluate(
      () => document.documentElement.scrollWidth - document.documentElement.clientWidth,
    );
    expect(overflow).toBeLessThanOrEqual(2);
  } finally {
    await ui.close();
  }
});
