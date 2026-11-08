# Web UI 浏览器测试

这里用 Playwright 驱动真实浏览器，覆盖人工操作流程；服务端逻辑仍由 Go
的 `httptest` 测试负责。Node 只出现在这个目录里，主程序构建与发版都不依
赖它。

## 运行

```sh
cd e2e
npm install
npx playwright install chromium
npm test
```

测试会先编译仓库根目录的 `sshmgr`，然后每个用例都生成一个全新的临时加
密库，以单机角色启动 `sshmgr ui`，从启动链接进入页面。桌面视口与手机视
口（375×667）都会运行同一组流程。

环境变量 `PATH` 在 Web UI 子进程里被指到空目录，这样 `sshmgr ui` 的「自动
打开系统浏览器」会失败并只打印启动链接，由 Playwright 自己使用这条一次
性链接，避免真实浏览器抢先消耗它。
