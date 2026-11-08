import { defineConfig } from "@playwright/test";

// 浏览器测试只在本机手动运行，不进入主持续集成；桌面与手机两种视口都
// 跑，覆盖项目页与授权表单在窄屏下的可用性。
export default defineConfig({
  globalSetup: "./global-setup.ts",
  testDir: "tests",
  timeout: 60_000,
  fullyParallel: false,
  workers: 1,
  reporter: [["list"]],
  use: {
    trace: "retain-on-failure",
  },
  projects: [
    {
      name: "desktop-chromium",
      use: { browserName: "chromium", viewport: { width: 1280, height: 800 } },
    },
    {
      name: "mobile-chromium",
      use: {
        browserName: "chromium",
        viewport: { width: 375, height: 667 },
        isMobile: true,
        hasTouch: true,
      },
    },
  ],
});
