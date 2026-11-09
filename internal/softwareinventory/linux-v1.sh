M=SSHMGR-INVENTORY
echo "$M BEGIN linux-v1"
echo "$M SECTION os"
cat /etc/os-release 2>&1
echo "kernel=$(uname -r)"
echo "machine=$(uname -m)"
echo "$M STATUS os 0"
echo "$M SECTION packages"
if command -v dpkg-query >/dev/null 2>&1; then
  dpkg-query -W -f='${Package}\t${Version}\t${Architecture}\t${db:Status-Abbrev}\n' 2>&1; echo "$M STATUS packages $?"
else echo "$M ABSENT packages"; fi
echo "$M SECTION listen"
if command -v ss >/dev/null 2>&1; then
  ss -H -tulnp 2>&1 | sed 's/[[:space:]]*$//'; echo "$M STATUS listen 0"
else echo "$M ABSENT listen"; fi
echo "$M SECTION exe"
for p in $(ss -H -tulnp 2>/dev/null | grep -o 'pid=[0-9]*' | cut -d= -f2 | sort -un); do
  printf '%s\t%s\n' "$p" "$(readlink /proc/$p/exe 2>&1)"
done
echo "$M STATUS exe 0"
echo "$M SECTION exe-owner"
if command -v dpkg-query >/dev/null 2>&1; then
  for e in $(ss -H -tulnp 2>/dev/null | grep -o 'pid=[0-9]*' | cut -d= -f2 | sort -un | while read p; do readlink /proc/$p/exe 2>/dev/null; done | sort -u); do
    printf '%s\t%s\n' "$e" "$(dpkg -S "$e" 2>/dev/null | head -n1 | cut -d: -f1)"
  done
  echo "$M STATUS exe-owner 0"
else echo "$M ABSENT exe-owner"; fi
echo "$M SECTION container-images"
if command -v docker >/dev/null 2>&1; then
  docker images --digests --no-trunc --format '{{.Repository}}\t{{.Tag}}\t{{.Digest}}\t{{.ID}}' 2>&1; echo "$M STATUS container-images $?"
else echo "$M ABSENT container-images"; fi
echo "$M SECTION containers"
if command -v docker >/dev/null 2>&1; then
  docker ps --no-trunc --format '{{.ID}}\t{{.Image}}\t{{.Names}}\t{{.Ports}}' 2>&1; echo "$M STATUS containers $?"
else echo "$M ABSENT containers"; fi
echo "$M SECTION firmware"
for f in bios_vendor bios_version bios_date board_vendor board_name sys_vendor product_name; do
  printf '%s\t%s\n' "$f" "$(cat /sys/class/dmi/id/$f 2>&1)"
done
echo "$M STATUS firmware 0"
echo "$M SECTION gpu-driver"
if [ -r /proc/driver/nvidia/version ] || command -v nvidia-smi >/dev/null 2>&1; then
  cat /proc/driver/nvidia/version 2>&1
  nvidia-smi --query-gpu=name,driver_version --format=csv,noheader 2>&1
  echo "$M STATUS gpu-driver 0"
else echo "$M ABSENT gpu-driver"; fi
echo "$M END"
