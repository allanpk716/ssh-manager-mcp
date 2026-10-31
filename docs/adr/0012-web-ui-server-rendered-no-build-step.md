# Web UI 前端:服务端渲染加 htmx,只有拓扑图页面用 Cytoscape.js,不引入前端构建步骤

仓库此前没有任何前端代码,工具链只有 Go,发版由 goreleaser 以 `CGO_ENABLED=0` 交叉编译。Web UI 首版要取代 TUI(主要是表单与列表),同时拓扑图页面是交互较重的画布(复合节点、拖动归类、缩放、视图过渡动画)。我们决定(2026-10-31 grilling 定案,地图「局域网共享存储、网络拓扑与 Web UI 管理界面」的票「Web UI:前端技术选型」):

1. **整站服务端渲染**:Go `html/template` 生成页面,htmx 做局部刷新;htmx 请求(带 `HX-Request` 头)只返回页面片段,否则返回整页。只有拓扑图页面嵌入 Cytoscape.js,配手写的原生 JavaScript 模块,经 `/ui/api/` 下的 JSON 接口读写拓扑数据、探测结果与布局坐标;这组接口只供拓扑图使用,不对外、不承诺兼容。其余页面不走 JSON 接口。
2. **不引入前端构建步骤**:不加 Node、npm、打包器,goreleaser 与发版流程不变。第三方库提交压缩后的发行文件进仓库,另有清单登记版本、来源网址与 SHA-256;运行时不从任何外部 CDN 加载,字体与图标也放本地。样式手写 CSS,用 CSS 自定义属性统一配色与间距,不用 Tailwind 之类需要生成步骤的方案。
3. **静态资源**用 `go:embed` 打进二进制,启动时按内容哈希生成带哈希的地址并长期缓存,HTML 一律不缓存。
4. **严格的内容安全策略**:`script-src`、`style-src`、`connect-src` 都只允许 `'self'`,禁止内联脚本、内联事件属性与内联样式;页面给 JavaScript 的数据放 `data-*` 属性或 `<script type="application/json">`;关闭 htmx 自带的内联指示样式,不用 `hx-on`。防跨站请求伪造令牌与会话绑定,表单用隐藏字段,htmx 请求由外部脚本从 `<meta>` 读出后放 `X-CSRF-Token` 请求头。
5. **数据刷新只用轮询**:需要时 htmx 定时拉取区块;耗时操作(如探测)做成「启动任务、轮询任务状态」。不做服务器推送事件。
6. **界面只出中文**,文字直接写在模板里,不做多语言框架。
7. **浏览器端测试用 Playwright**,Node 只限于仓库的 `e2e/` 目录与一个独立的 CI 工作流,构建与发版都不依赖它;服务端逻辑用 Go `httptest` 测。

## Considered Options

- **整站单页应用(React、Svelte 或 Vue)**:被否——要多维护一套 JSON 接口与前端状态,还要引入 Node 构建链进发版;取代 TUI 的表单与列表用不上它的好处。
- **纯服务端渲染、不用 htmx**:被否——审计、备份状态、探测结果这类常刷新的页面整页刷新体验差。
- **用纯 Go 的 chromedp 做浏览器测试**:被否——写法啰嗦、生态小;Node 被限制在测试侧,不影响「发版不依赖 Node」。

## Consequences

- 拓扑图的 JavaScript 没有 TypeScript 类型检查,可选用 JSDoc 加 `tsc --checkJs` 在本地检查,不进发版流程。
- 第三方库升级靠手工整文件替换并更新清单的校验值。
