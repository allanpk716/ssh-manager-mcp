#!/usr/bin/env bash
set -euo pipefail

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
docker rm -f sshmgr-tutorial-demo >/dev/null 2>&1 || true

if [[ -f "$script_dir/.demo-root" ]]; then
  demo_root=$(cat "$script_dir/.demo-root")
  if [[ -d "$demo_root" ]]; then
    find "$demo_root" -type f -delete
    find "$demo_root" -type l -delete
    echo "已删除临时目录中的文件；目录本身保留：$demo_root"
  fi
fi

find "$script_dir" -maxdepth 1 -type f \( -name '.demo-root' -o -name '.demo-env' \) -delete
echo "演示清理完成。"
