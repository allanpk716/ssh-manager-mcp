import {
  expect,
  gotoUIPath,
  openStandaloneUI,
  startStandaloneUIWithTopology,
  test,
} from "../helpers";

async function openBatchLocationList(page: import("@playwright/test").Page) {
  await gotoUIPath(page, "/ui/servers/?query=batch-");
  await expect(page.locator("[data-server-card]")).toHaveCount(5);
  await page.getByRole("button", { name: "多选" }).click();
}

test("批量设置地点：选三台服务器（两台同机）确认后一次更新", async ({ page, isMobile }, testInfo) => {
  testInfo.skip(isMobile, "这个用例按桌面视口验证完整确认流程");
  const ui = await startStandaloneUIWithTopology(undefined, undefined, {
    SSHMGR_E2E_SERVER_BATCH_LOCATION: "1",
  });
  try {
    await openStandaloneUI(page, ui);
    await openBatchLocationList(page);
    await page.getByLabel("选择 batch-a-web").check();
    await page.getByLabel("选择 batch-a-db").check();
    await page.getByLabel("选择 batch-b-tool").check();
    await expect(page.locator("[data-server-selected-count]")).toHaveText("已选 3 台");

    const setLocation = page.getByRole("button", { name: "设置地点" });
    await expect(setLocation).toBeEnabled();
    await setLocation.click();
    const drawer = page.locator("[data-server-location-drawer]");
    await expect(drawer).toBeVisible();
    const next = page.getByRole("button", { name: "查看后果" });
    await expect(next).toBeDisabled();
    await page.getByRole("radio", { name: "园区A / 一号机房" }).check();
    await expect(next).toBeEnabled();
    await next.click();

    await expect(page.getByRole("heading", { name: "确认批量设置地点" })).toBeVisible();
    await expect(page.getByText("将把 3 台服务器所在的机器地点设为 园区A / 一号机房。")).toBeVisible();
    await expect(page.getByText("同一台机器上的其他服务器条目也会一起变化：batch-a-extra")).toBeVisible();
    const deviceB = page.locator(".batch-location-device", { hasText: "批量机器乙" });
    await expect(deviceB).toContainText("当前地点：未登记地点");
    await expect(deviceB).toContainText("也会一起变化：无");

    // 上一步回到原列表并重新打开地点弹层，筛选与选择都保留，可以重选地点。
    await page.getByRole("link", { name: "上一步" }).click();
    await expect(page).toHaveURL(/\/ui\/servers\/\?query=batch-$/);
    await expect(page.locator("[data-server-selected-count]")).toHaveText("已选 3 台");
    await expect(page.locator("[data-server-location-drawer]")).toBeVisible();
    await page.getByRole("radio", { name: "园区A / 一号机房" }).check();
    await page.getByRole("button", { name: "查看后果" }).click();
    await expect(page.getByRole("heading", { name: "确认批量设置地点" })).toBeVisible();

    await page.getByRole("button", { name: "确认修改" }).click();

    await expect(page).toHaveURL(/\/ui\/servers\/\?query=batch-/);
    await expect(page.locator("[data-server-selected-count]")).toHaveText("已选 3 台");
    for (const name of ["batch-a-web", "batch-a-db", "batch-a-extra", "batch-b-tool"]) {
      const card = page.locator("[data-server-card]", { hasText: name });
      await expect(card.locator(".server-card-context")).toContainText("园区A / 一号机房");
    }
  } finally {
    await ui.close();
  }
});

test("批量设置地点：未关联机器单列且不阻塞其余机器写入", async ({ page, isMobile }, testInfo) => {
  testInfo.skip(isMobile, "这个用例按桌面视口验证未关联机器的确认页");
  const ui = await startStandaloneUIWithTopology(undefined, undefined, {
    SSHMGR_E2E_SERVER_BATCH_LOCATION: "1",
  });
  try {
    await openStandaloneUI(page, ui);
    await openBatchLocationList(page);
    await page.getByLabel("选择 batch-no-device").check();
    await page.getByLabel("选择 batch-b-tool").check();
    await page.getByRole("button", { name: "设置地点" }).click();
    await page.getByRole("radio", { name: "未登记地点" }).check();
    await page.getByRole("button", { name: "查看后果" }).click();

    await expect(page.getByRole("heading", { name: "确认批量设置地点" })).toBeVisible();
    await expect(page.getByText("将把 1 台服务器所在的机器地点设为 未登记地点。")).toBeVisible();
    await expect(
      page.locator(".batch-location-unassociated", { hasText: "batch-no-device" }),
    ).toBeVisible();
    await page.getByRole("button", { name: "确认修改" }).click();

    await expect(page).toHaveURL(/\/ui\/servers\/\?query=batch-/);
    await expect(page.locator("[data-server-selected-count]")).toHaveText("已选 2 台");
    const toolCard = page.locator("[data-server-card]", { hasText: "batch-b-tool" });
    await expect(toolCard.locator(".server-card-context")).toContainText("地点未登记");
  } finally {
    await ui.close();
  }
});
