#!/usr/bin/env bash
set -euo pipefail

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
repo_root=$(cd "$script_dir/../../../.." && pwd)
container_name=sshmgr-tutorial-demo

for program in go docker mktemp python3 vhs ttyd ffmpeg lsof; do
  command -v "$program" >/dev/null 2>&1 || {
    echo "缺少 $program；请先安装或加入 PATH。" >&2
    exit 1
  }
done

demo_root=$(mktemp -d "${TMPDIR:-/tmp}/sshmgr-tutorial.XXXXXXXX")
mkdir -p \
  "$demo_root/bin" \
  "$demo_root/home" \
  "$demo_root/workspace" \
  "$demo_root/config" \
  "$demo_root/vault-root" \
  "$demo_root/cache-dek"

(cd "$repo_root" && go build -o "$demo_root/bin/sshmgr" ./cmd/sshmgr)
ln -s "$demo_root/bin/sshmgr" "$demo_root/workspace/sshmgr"

cat >"$script_dir/.demo-root" <<EOF
$demo_root
EOF

cat >"$script_dir/.demo-env" <<EOF
DEMO_ROOT='$demo_root'
export DEMO_ROOT
SAVED_HOME="\${SAVED_HOME:-\$HOME}"
export SAVED_HOME
export SSHMGR_VAULT_ROOT="\$DEMO_ROOT/vault-root"
export SSHMGR_STORE="\$DEMO_ROOT/vault-root/ssh-manager/store.db"
export SSHMGR_FILEKEY_PATH="\$DEMO_ROOT/master.key.plain"
export SSHMGR_CONFIG_ROOT="\$DEMO_ROOT/config"
export SSHMGR_CACHE_DEK_DIR="\$DEMO_ROOT/cache-dek"
export HOME="\$DEMO_ROOT/home"
export PS1='\$ '
export PATH="\$DEMO_ROOT/bin:\$PATH"
cd "\$DEMO_ROOT/workspace" || exit 1
EOF

# The workspace copy keeps Claude Code's normal authentication while the
# terminal prompt and sshmgr binary stay inside the demo directory.
cp "$script_dir/.demo-env" "$demo_root/workspace/.demo-env"
cat >"$demo_root/workspace/.claude-env" <<'EOF'
source ./.demo-env
export HOME="$SAVED_HOME"
EOF

cat >"$demo_root/workspace/prepare-claude.sh" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
./sshmgr projects rotate my-agent > rotate.txt
python3 - "$DEMO_ROOT" <<'PY'
import json
import os
import re
import sys

root = sys.argv[1]
text = open("rotate.txt", encoding="utf-8").read()
match = re.search(r'\{"mcpServers".*\}', text)
if match is None:
    raise SystemExit("cannot find project token JSON")
token = json.loads(match.group(0))["mcpServers"]["ssh"]["env"]["SSHMGR_TOKEN"]
config = {
    "mcpServers": {
        "ssh": {
            "command": os.path.join(root, "bin", "sshmgr"),
            "args": ["mcp"],
            "env": {
                "SSHMGR_TOKEN": token,
                "SSHMGR_VAULT_ROOT": os.path.join(root, "vault-root"),
                "SSHMGR_STORE": os.path.join(
                    root, "vault-root", "ssh-manager", "store.db"
                ),
                "SSHMGR_FILEKEY_PATH": os.path.join(root, "master.key.plain"),
                "SSHMGR_CONFIG_ROOT": os.path.join(root, "config"),
                "SSHMGR_CACHE_DEK_DIR": os.path.join(root, "cache-dek"),
            },
        }
    }
}
with open(".mcp.json", "w", encoding="utf-8") as output:
    json.dump(config, output, indent=2)
    output.write("\n")
PY
EOF
chmod 700 "$demo_root/workspace/prepare-claude.sh"

docker rm -f "$container_name" >/dev/null 2>&1 || true

if lsof -nP -iTCP:2222 -sTCP:LISTEN >/dev/null 2>&1; then
  echo "127.0.0.1:2222 已被占用；先停止占用进程，或修改本脚本和教程端口。" >&2
  exit 1
fi

docker run -d \
  --name "$container_name" \
  -e PUID=1000 \
  -e PGID=1000 \
  -e TZ=Asia/Shanghai \
  -e PASSWORD_ACCESS=true \
  -e USER_PASSWORD=deploy-password \
  -e USER_NAME=deploy \
  -p 127.0.0.1:2222:2222 \
  lscr.io/linuxserver/openssh-server:latest >/dev/null

ready=false
for _ in $(seq 1 30); do
  if docker logs "$container_name" 2>&1 | grep -q '\[ls.io-init\] done'; then
    ready=true
    break
  fi
  sleep 1
done
if [[ "$ready" != true ]]; then
  echo "演示容器 30 秒内没有完成启动；请查看 docker logs $container_name。" >&2
  exit 1
fi

echo "演示环境已就绪：$demo_root"
echo "接下来在 $script_dir 按文件名顺序运行 vhs。"
