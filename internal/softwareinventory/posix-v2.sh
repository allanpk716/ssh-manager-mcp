M=SSHMGR-INVENTORY
PATH="$PATH:/usr/sbin:/sbin:/usr/local/bin:/opt/homebrew/bin:/usr/syno/bin:/usr/syno/sbin"
AS=""
if [ "$(uname -s)" = Darwin ] && [ "$(id -u)" = 0 ]; then AS="sudo -u $(stat -f %Su /dev/console) -H"; fi
echo "$M BEGIN posix-v2"
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
  for a in /Applications/*.app /Applications/Utilities/*.app; do
    [ -d "$a" ] || continue
    printf '%s\t%s\t%s\n' "$a" "$(defaults read "$a/Contents/Info" CFBundleShortVersionString 2>/dev/null)" "$(defaults read "$a/Contents/Info" CFBundleIdentifier 2>/dev/null)"
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
echo "$M SECTION containers"
if command -v docker >/dev/null 2>&1; then
  $AS docker ps --no-trunc --format '{{.ID}}\t{{.Image}}\t{{.Names}}\t{{.Ports}}' 2>&1; echo "$M STATUS containers $?"
else echo "$M ABSENT containers"; fi
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
echo "$M END"
