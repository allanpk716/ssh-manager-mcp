M=SSHMGR-INVENTORY
PATH="$PATH:/usr/sbin:/sbin:/usr/local/bin:/opt/homebrew/bin:/usr/syno/bin:/usr/syno/sbin"
AS=""
if [ "$(uname -s)" = Darwin ] && [ "$(id -u)" = 0 ]; then AS="sudo -u $(stat -f %Su /dev/console) -H"; fi
echo "$M BEGIN posix-v3"
echo "$M SECTION os"
echo "kernel-name=$(uname -s)"
echo "kernel=$(uname -r)"
echo "machine=$(uname -m)"
for f in /etc/os-release /etc/openwrt_release /etc.defaults/VERSION; do
  if [ -r "$f" ]; then echo "file=$f"; cat "$f" 2>&1; fi
done
if command -v sw_vers >/dev/null 2>&1; then echo "file=sw_vers"; sw_vers 2>&1; fi
echo "$M STATUS os 0"
echo "$M SECTION packages"
if [ ! -r /etc.defaults/VERSION ] && command -v dpkg-query >/dev/null 2>&1; then
  dpkg-query -W -f='${Package}\t${Version}\t${Architecture}\t${db:Status-Abbrev}\n' 2>&1; echo "$M STATUS packages $?"
else echo "$M ABSENT packages"; fi
echo "$M SECTION listen"
if command -v ss >/dev/null 2>&1; then
  ss -H -tulnp 2>&1 | sed 's/[[:space:]]*$//'; echo "$M STATUS listen 0"
else echo "$M ABSENT listen"; fi
echo "$M SECTION listen-netstat"
if ! command -v ss >/dev/null 2>&1 && [ "$(uname -s)" = Linux ] && command -v netstat >/dev/null 2>&1; then
  netstat -tulnp 2>&1 | sed 's/[[:space:]]*$//'; echo "$M STATUS listen-netstat 0"
else echo "$M ABSENT listen-netstat"; fi
echo "$M SECTION listen-lsof"
if [ "$(uname -s)" = Darwin ] && command -v lsof >/dev/null 2>&1; then
  lsof -nP -iTCP -sTCP:LISTEN -iUDP -FpcnPt 2>&1; echo "$M STATUS listen-lsof 0"
else echo "$M ABSENT listen-lsof"; fi
echo "$M SECTION exe"
if [ -d /proc/self ]; then
  for p in $( { ss -H -tulnp 2>/dev/null; netstat -tulnp 2>/dev/null; } | grep -oE 'pid=[0-9]+|[0-9]+/' | tr -dc '0-9\n' | sort -un); do
    printf '%s\t%s\n' "$p" "$(readlink /proc/$p/exe 2>&1)"
  done
elif [ "$(uname -s)" = Darwin ]; then
  for p in $(lsof -nP -iTCP -sTCP:LISTEN -iUDP -Fp 2>/dev/null | sed -n 's/^p//p' | sort -un); do
    printf '%s\t%s\n' "$p" "$(ps -o comm= -p "$p" 2>&1)"
  done
fi
echo "$M STATUS exe 0"
echo "$M SECTION exe-owner"
if [ ! -r /etc.defaults/VERSION ] && command -v dpkg-query >/dev/null 2>&1; then
  for e in $(ss -H -tulnp 2>/dev/null | grep -o 'pid=[0-9]*' | cut -d= -f2 | sort -un | while read p; do readlink /proc/$p/exe 2>/dev/null; done | sort -u); do
    printf '%s\t%s\n' "$e" "$(dpkg -S "$e" 2>/dev/null | head -n1 | cut -d: -f1)"
  done
  echo "$M STATUS exe-owner 0"
else echo "$M ABSENT exe-owner"; fi
echo "$M SECTION apps"
if [ "$(uname -s)" = Darwin ]; then
  for app_directory in /Applications /Applications/Utilities; do
    [ -d "$app_directory" ] || continue
    /bin/sh -c 'for a in "$1"/*.app; do
      [ -d "$a" ] || continue
      printf "%s\t%s\t%s\n" "$a" "$(defaults read "$a/Contents/Info" CFBundleShortVersionString 2>/dev/null)" "$(defaults read "$a/Contents/Info" CFBundleIdentifier 2>/dev/null)"
    done' sh "$app_directory"
  done
  echo "$M STATUS apps 0"
else echo "$M ABSENT apps"; fi
echo "$M SECTION brew"
if command -v brew >/dev/null 2>&1; then
  $AS brew list --versions --formula 2>&1 | sed 's/^/formula\t/'; $AS brew list --versions --cask 2>&1 | sed 's/^/cask\t/'; echo "$M STATUS brew 0"
else echo "$M ABSENT brew"; fi
echo "$M SECTION synology-packages"
if command -v synopkg >/dev/null 2>&1; then
  synopkg list 2>&1; echo "$M STATUS synology-packages $?"
else echo "$M ABSENT synology-packages"; fi
echo "$M SECTION container-images"
if command -v docker >/dev/null 2>&1; then
  $AS docker images --digests --no-trunc --format '{{.Repository}}\t{{.Tag}}\t{{.Digest}}\t{{.ID}}' 2>&1; echo "$M STATUS container-images $?"
else echo "$M ABSENT container-images"; fi
echo "$M SECTION containers-v3"
if command -v docker >/dev/null 2>&1; then
  $AS docker ps -a --no-trunc --format '{{.ID}}\t{{.Image}}\t{{.Names}}\t{{.Ports}}\t{{.State}}' 2>&1; echo "$M STATUS containers-v3 $?"
else echo "$M ABSENT containers-v3"; fi
echo "$M SECTION container-image-refs"
if command -v docker >/dev/null 2>&1; then
  container_ids=$($AS docker ps -aq 2>/dev/null)
  if [ -n "$container_ids" ]; then
    $AS docker inspect --format '{{.Id}}\t{{.Image}}' $container_ids 2>&1
  fi
  echo "$M STATUS container-image-refs $?"
else echo "$M ABSENT container-image-refs"; fi
echo "$M SECTION container-image-labels"
if command -v docker >/dev/null 2>&1; then
  image_ids=$($AS docker images -q --no-trunc 2>/dev/null | sort -u)
  if [ -n "$image_ids" ]; then
    $AS docker image inspect --format '{{.Id}}\t{{if .Config.Labels}}{{index .Config.Labels "org.opencontainers.image.version"}}{{end}}' $image_ids 2>&1
  fi
  echo "$M STATUS container-image-labels $?"
else echo "$M ABSENT container-image-labels"; fi
echo "$M SECTION firmware"
if [ -d /sys/class/dmi/id ]; then
  for f in bios_vendor bios_version bios_date board_vendor board_name sys_vendor product_name; do
    printf '%s\t%s\n' "$f" "$(cat /sys/class/dmi/id/$f 2>&1)"
  done
  echo "$M STATUS firmware 0"
else echo "$M ABSENT firmware"; fi
echo "$M SECTION gpu-driver"
if [ -r /proc/driver/nvidia/version ] || command -v nvidia-smi >/dev/null 2>&1; then
  cat /proc/driver/nvidia/version 2>&1
  nvidia-smi --query-gpu=name,driver_version --format=csv,noheader 2>&1
  echo "$M STATUS gpu-driver 0"
else echo "$M ABSENT gpu-driver"; fi
echo "$M SECTION pending-updates-apt"
# apt list --upgradable 只读取本地软件包索引，不刷新索引、不安装升级。
if command -v apt >/dev/null 2>&1; then
  apt_pending=$(LC_ALL=C apt list --upgradable 2>/dev/null); apt_rc=$?
  if [ "$apt_rc" -eq 0 ]; then
    printf '%s\n' "$apt_pending"
    printf 'cache-time\t%s\n' "$(stat -c %Y /var/lib/apt/lists 2>/dev/null)"
    echo "$M STATUS pending-updates-apt 0"
  else
    echo "$M STATUS pending-updates-apt $apt_rc"
  fi
else echo "$M ABSENT pending-updates-apt"; fi
echo "$M SECTION pending-updates-dnf"
# dnf/yum 使用 --cacheonly 只读本地元数据，并用 --security 只取安全更新。
if command -v dnf >/dev/null 2>&1; then
  dnf_pending=$(dnf -q -C --security check-update 2>/dev/null); dnf_rc=$?
  if [ "$dnf_rc" -eq 0 ] || [ "$dnf_rc" -eq 100 ]; then
    printf '%s\n' "$dnf_pending"
    printf 'cache-time\t%s\n' "$(stat -c %Y /var/cache/dnf 2>/dev/null)"
    echo "$M STATUS pending-updates-dnf 0"
  else
    echo "$M STATUS pending-updates-dnf $dnf_rc"
  fi
elif command -v yum >/dev/null 2>&1; then
  yum_pending=$(yum -q -C --security check-update 2>/dev/null); yum_rc=$?
  if [ "$yum_rc" -eq 0 ] || [ "$yum_rc" -eq 100 ]; then
    printf '%s\n' "$yum_pending"
    printf 'cache-time\t%s\n' "$(stat -c %Y /var/cache/yum 2>/dev/null)"
    echo "$M STATUS pending-updates-dnf 0"
  else
    echo "$M STATUS pending-updates-dnf $yum_rc"
  fi
else echo "$M ABSENT pending-updates-dnf"; fi
echo "$M END"
