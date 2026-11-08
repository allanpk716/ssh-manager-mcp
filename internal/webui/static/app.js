// Web UI 初始化脚本：把页面 meta 里的防跨站请求伪造令牌放进每个 htmx
// 请求的 X-CSRF-Token 请求头，页面模板本身保持无内联脚本。
(function () {
  "use strict";
  var meta = document.querySelector('meta[name="csrf-token"]');
  var token = meta === null ? "" : meta.getAttribute("content");
  document.body.addEventListener("htmx:configRequest", function (event) {
    event.detail.headers["X-CSRF-Token"] = token;
  });
  document.body.addEventListener("click", function (event) {
    var button = event.target.closest("[data-copy-target]");
    if (button === null) {
      return;
    }
    var source = document.getElementById(button.getAttribute("data-copy-target"));
    if (source === null) {
      return;
    }
    var text = source.textContent;
    function finished() {
      var oldText = button.textContent;
      button.textContent = "已复制";
      window.setTimeout(function () {
        button.textContent = oldText;
      }, 1500);
    }
    if (navigator.clipboard && navigator.clipboard.writeText) {
      navigator.clipboard.writeText(text).then(finished, function () {
        window.prompt("请手动复制：", text);
      });
      return;
    }
    window.prompt("请手动复制：", text);
  });
})();
