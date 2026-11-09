// 机密项授权矩阵的浏览器辅助：只改变当前页面可见格子的勾选状态，
// 不发送请求，也不保存任何授权。
(function () {
  "use strict";
  document.addEventListener("click", function (event) {
    var button = event.target.closest("[data-matrix-action]");
    if (button === null) {
      return;
    }
    var matrix = button.closest("[data-secret-matrix]");
    if (matrix === null) {
      return;
    }
    var action = button.getAttribute("data-matrix-action");
    var checked = action.indexOf("-check") !== -1;
    if (action.indexOf("row-") === 0) {
      setCells(matrix, "[data-secret-cell='" + cssEscape(button.getAttribute("data-row")) + "']", checked);
      return;
    }
    if (action.indexOf("column-") === 0) {
      setCells(matrix, "[data-server-cell='" + cssEscape(button.getAttribute("data-column")) + "']", checked);
      return;
    }
    if (action.indexOf("intersection-") === 0) {
      var selectedRows = selectedValues(matrix, "[data-row-selector]");
      var selectedColumns = selectedValues(matrix, "[data-column-selector]");
      if (selectedRows.length === 0 || selectedColumns.length === 0) {
        return;
      }
      matrix.querySelectorAll("[data-secret-cell]").forEach(function (cell) {
        if (
          selectedRows.indexOf(cell.getAttribute("data-secret-cell")) !== -1 &&
          selectedColumns.indexOf(cell.getAttribute("data-server-cell")) !== -1
        ) {
          cell.checked = checked;
        }
      });
    }
  });

  function setCells(matrix, selector, checked) {
    matrix.querySelectorAll(selector).forEach(function (cell) {
      cell.checked = checked;
    });
  }

  function selectedValues(matrix, selector) {
    var values = [];
    matrix.querySelectorAll(selector).forEach(function (input) {
      if (input.checked) {
        values.push(input.value);
      }
    });
    return values;
  }

  function cssEscape(value) {
    if (window.CSS && window.CSS.escape) {
      return window.CSS.escape(value);
    }
    return value.replace(/[^a-zA-Z0-9_-]/g, "\\$&");
  }
})();
