#!/usr/bin/env bash
# cloudmail-bridge 一键卸载
# 用法：bash uninstall.sh [安装目录] [--dry-run] [-y]
# 不传安装目录则用当前目录。--dry-run 只打印不动手。
#
# 只删本项目的东西，不动 cloud-mail 服务器上的邮件、不动别的容器和证书。

set -uo pipefail

DRY=0; YES=0; DIR=""
for a in "$@"; do
  case "$a" in
    --dry-run) DRY=1 ;;
    -y|--yes)  YES=1 ;;
    -h|--help) sed -n '2,7p' "$0"; exit 0 ;;
    -*)        echo "未知参数：$a" >&2; exit 1 ;;
    *)         DIR="$a" ;;
  esac
done
DIR="${DIR:-$(pwd)}"
[ -d "$DIR" ] || { echo "目录不存在：$DIR" >&2; exit 1; }
ACME="$HOME/.acme.sh/acme.sh"

# 容器名：优先读 compose 里的 container_name
NAME=cloudmail-bridge
for f in docker-compose.yml docker-compose.yaml compose.yml compose.yaml; do
  if [ -f "$DIR/$f" ]; then
    N=$(grep -E '^\s*container_name\s*:' "$DIR/$f" | head -1 | sed 's/.*:[[:space:]]*//' | tr -d '"'"'"' ')
    [ -n "$N" ] && { NAME="$N"; break; }
  fi
done

IMAGE=$(docker inspect -f '{{.Config.Image}}' "$NAME" 2>/dev/null || true)

# 本项目证书：续期配置里写的是本目录的 certs/；不是本项目的绝不碰
DOMAINS=""
if [ -x "$ACME" ]; then
  DOMAINS=$(grep -lF "Le_RealCertPath='$DIR/certs" "$HOME"/.acme.sh/*/*.conf 2>/dev/null \
            | xargs -r -n1 dirname 2>/dev/null | xargs -r -n1 basename 2>/dev/null \
            | sed 's/_ecc$//' | sort -u | tr '\n' ' ')
fi
MYCOUNT=$(echo $DOMAINS | wc -w)
LEFT=$([ -x "$ACME" ] && "$ACME" --list 2>/dev/null | tail -n +2 | grep -c . || echo 0)

echo "安装目录 : $DIR"
echo "容器     : $NAME"
echo "镜像     : ${IMAGE:-（无）}"
echo "证书续期 : ${DOMAINS:-（无）}"
echo ""
echo "将执行："
echo "  1) docker rm -f $NAME"
[ -n "$IMAGE" ] && echo "  2) docker rmi $IMAGE"
if [ -n "$DOMAINS" ]; then
  echo "  3) acme.sh --remove -d <域名> --ecc  （$DOMAINS）"
  [ "$((LEFT - MYCOUNT))" -le 0 ] && echo "     并移除 acme.sh 定时任务（本机已无其他证书）"
fi
echo "  4) rm -rf $DIR"
echo ""
echo "不会动：cloud-mail 服务器上的邮件、其他容器与镜像、其他域名证书。"
echo "卸载后请自行关闭：路由器端口转发、安全组放行、不再使用的 DNS 记录。"
echo ""

if [ "$DRY" = 1 ]; then echo "演练结束，未做任何改动。"; exit 0; fi
if [ "$YES" != 1 ]; then
  printf "确认卸载请输入 yes："; read -r C
  [ "$C" = yes ] || { echo "已取消。"; exit 0; }
fi
echo ""

# 1) 容器
docker rm -f "$NAME" >/dev/null 2>&1 && echo "[完成] 容器已删除" || echo "[跳过] 容器不存在"

# 2) 镜像
if [ -n "$IMAGE" ]; then
  docker rmi "$IMAGE" >/dev/null 2>&1 && echo "[完成] 镜像已删除" \
    || echo "[注意] 镜像删除失败，可能被其他容器占用"
fi

# 3) 证书续期（只注销指向本目录的，然后仅在无其他证书时清 crontab）
if [ -n "$DOMAINS" ]; then
  for d in $DOMAINS; do
    "$ACME" --remove -d "$d" --ecc >/dev/null 2>&1 && echo "[完成] 已注销 $d 的续期" \
      || echo "[注意] 注销 $d 失败，可手动执行：$ACME --remove -d $d --ecc"
  done
  if [ "$("$ACME" --list 2>/dev/null | tail -n +2 | grep -c . || echo 0)" -le 0 ]; then
    crontab -l 2>/dev/null | grep -v acme.sh | crontab - 2>/dev/null \
      && echo "[完成] 已移除 acme.sh 定时任务"
  else
    echo "[跳过] 还有其他证书，保留 acme.sh 定时任务"
  fi
fi

# 4) 目录（data/ 由容器以 root 写入，普通用户删不掉，需要 sudo）
if rm -rf "$DIR" 2>/dev/null; then
  echo "[完成] 安装目录已删除"
elif command -v sudo >/dev/null 2>&1 && sudo rm -rf "$DIR"; then
  echo "[完成] 安装目录已删除（sudo）"
else
  echo "[错误] 目录删除失败，请手动执行：sudo rm -rf $DIR" >&2
  exit 1
fi

echo ""
echo "卸载完成。"
