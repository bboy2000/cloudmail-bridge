#!/usr/bin/env bash
# cloudmail-bridge 一键卸载脚本
#
# 做的事：停容器 -> 删容器 -> 删镜像 -> 清理本项目的证书续期 -> 删安装目录
# 不做的事：不碰 cloud-mail 服务器上的任何邮件，不碰别的项目的证书和容器
#
# 用法：
#   bash uninstall.sh                  # 在安装目录里执行，交互确认
#   bash uninstall.sh /path/to/install # 指定安装目录
#   bash uninstall.sh --dry-run        # 只打印将执行的操作，不动任何东西
#   bash uninstall.sh -y               # 跳过确认（谨慎）
#   bash uninstall.sh --keep-data      # 保留 data/（已删邮件记录）再卸

set -uo pipefail

DRY_RUN=0
ASSUME_YES=0
KEEP_DATA=0
TARGET=""

C_RED=''; C_GREEN=''; C_YELLOW=''; C_BLUE=''; C_OFF=''
if [ -t 1 ]; then
  C_RED='\033[31m'; C_GREEN='\033[32m'; C_YELLOW='\033[33m'; C_BLUE='\033[36m'; C_OFF='\033[0m'
fi

info() { printf "${C_BLUE}[信息]${C_OFF} %s\n" "$*"; }
ok()   { printf "${C_GREEN}[完成]${C_OFF} %s\n" "$*"; }
warn() { printf "${C_YELLOW}[注意]${C_OFF} %s\n" "$*"; }
err()  { printf "${C_RED}[错误]${C_OFF} %s\n" "$*" >&2; }

usage() {
  cat <<'USAGE'
cloudmail-bridge 卸载脚本

用法:
  bash uninstall.sh [安装目录] [选项]

选项:
  --dry-run      只打印将要执行的操作，不做任何改动
  --keep-data    保留 data/ 目录（客户端已删邮件记录），其余照删
  -y, --yes      跳过交互确认
  -h, --help     显示本帮助
USAGE
}

for arg in "$@"; do
  case "$arg" in
    --dry-run)   DRY_RUN=1 ;;
    --keep-data) KEEP_DATA=1 ;;
    -y|--yes)    ASSUME_YES=1 ;;
    -h|--help)   usage; exit 0 ;;
    -*)          err "未知参数：$arg"; usage; exit 1 ;;
    *)           TARGET="$arg" ;;
  esac
done

DIR="${TARGET:-$(pwd)}"
if [ ! -d "$DIR" ]; then
  err "目录不存在：$DIR"
  exit 1
fi

# 定位 compose 文件（支持 docker-compose.yml / compose.yaml 等）
COMPOSE=""
for f in docker-compose.yml docker-compose.yaml compose.yml compose.yaml; do
  if [ -f "$DIR/$f" ]; then COMPOSE="$DIR/$f"; break; fi
done

CONTAINER="cloudmail-bridge"
# 从 compose 文件里读出实际容器名（有 container_name 就用它）
if [ -n "$COMPOSE" ]; then
  CN=$(grep -E '^\s*container_name\s*:' "$COMPOSE" | head -1 | sed 's/.*:\s*//' | tr -d '"'"'"' ')
  [ -n "$CN" ] && CONTAINER="$CN"
fi

echo ""
echo "=============================================="
echo "  cloudmail-bridge 卸载"
echo "=============================================="
echo ""
info "安装目录：$DIR"
[ -n "$COMPOSE" ] && info "编排文件：$COMPOSE" || warn "目录内未找到 compose 文件"
info "容器名  ：$CONTAINER"
[ "$DRY_RUN" = "1" ] && warn "【演练模式】只打印，不实际执行"
echo ""

# ---------- 收集现状 ----------
CONTAINER_EXISTS=0
IMAGE=""
if command -v docker >/dev/null 2>&1; then
  if docker inspect "$CONTAINER" >/dev/null 2>&1; then
    CONTAINER_EXISTS=1
    IMAGE=$(docker inspect -f '{{.Config.Image}}' "$CONTAINER" 2>/dev/null || true)
  fi
fi

# 找出证书续期配置指向本目录的条目（不按域名硬编码，避免误删别的项目）
ACME_HOME="$HOME/.acme.sh"
ACME_BIN="$ACME_HOME/acme.sh"
ACME_DOMAINS=""
REMAIN_CERTS=0
if [ -x "$ACME_BIN" ]; then
  # 两个 glob 会重复命中同一个 conf，先去重再拼成空格分隔的列表
  _dom_list=$(mktemp)
  for conf in "$ACME_HOME"/*_ecc/*.conf "$ACME_HOME"/*/*.conf; do
    [ -f "$conf" ] || continue
    if grep -qF "Le_RealCertPath='$DIR/certs" "$conf" 2>/dev/null; then
      d=$(basename "$(dirname "$conf")")
      echo "${d%_ecc}" >> "$_dom_list"
    fi
  done
  ACME_DOMAINS=$(sort -u "$_dom_list" 2>/dev/null | tr '\n' ' ')
  rm -f "$_dom_list"
  REMAIN_CERTS=$("$ACME_BIN" --list 2>/dev/null | tail -n +2 | grep -c . || true)
fi
ACME_COUNT=0
[ -n "$ACME_DOMAINS" ] && ACME_COUNT=$(echo $ACME_DOMAINS | wc -w)

echo "将要执行："
[ "$CONTAINER_EXISTS" = "1" ] && echo "  1) 停止并删除容器 $CONTAINER" || echo "  1) （容器不存在，跳过）"
if [ -n "$IMAGE" ]; then echo "  2) 删除镜像 $IMAGE"; else echo "  2) （无关联镜像，跳过）"; fi
if [ -n "$ACME_DOMAINS" ]; then
  echo "  3) 注销证书续期：$ACME_DOMAINS"
  REMAIN_AFTER=$((REMAIN_CERTS - ACME_COUNT))
  if [ "$REMAIN_AFTER" -le 0 ]; then
    echo "     并移除 acme.sh 的每日续期任务（没有其他证书了）"
  else
    echo "     保留 acme.sh 续期任务（还有 $REMAIN_AFTER 张别的证书在用）"
  fi
else
  echo "  3) （未发现指向本目录的证书续期，跳过）"
fi
if [ "$KEEP_DATA" = "1" ]; then
  echo "  4) 删除安装目录 $DIR（保留 data/）"
else
  echo "  4) 删除安装目录 $DIR（含 data/ 已删邮件记录）"
fi
echo ""
warn "不可撤销：以上操作执行后无法自动恢复。"
warn "不会动：cloud-mail 服务器上的邮件、其他 Docker 容器与镜像、其他域名证书。"
echo ""
warn "卸载后还需你手动处理（脚本无权改）："
echo "  - 路由器端口转发规则（IMAP/SMTP）"
echo "  - 云服务器安全组放行规则"
echo "  - DNS 解析记录（如不再使用该域名）"
echo ""

if [ "$DRY_RUN" = "1" ]; then
  ok "演练结束，未做任何改动。去掉 --dry-run 即可实际卸载。"
  exit 0
fi

if [ "$ASSUME_YES" != "1" ]; then
  printf "${C_GREEN}>>> ${C_OFF}确认卸载请输入 yes 并回车（其他任意输入取消）："
  read -r CONFIRM
  if [ "$CONFIRM" != "yes" ]; then
    info "已取消，未做任何改动。"
    exit 0
  fi
fi

# ---------- 1) 停并删容器 ----------
echo ""
if [ "$CONTAINER_EXISTS" = "1" ] || [ -n "$COMPOSE" ]; then
  info "第 1 步：停止并删除容器 ..."
  if [ -n "$COMPOSE" ]; then
    # 用 compose 删，能把网络等附属资源一起清掉
    ( cd "$DIR" && docker compose down --remove-orphans ) >/dev/null 2>&1 \
      && ok "容器已删除" || { docker rm -f "$CONTAINER" >/dev/null 2>&1 && ok "容器已删除" || err "删除容器失败，请手动检查"; }
  else
    docker rm -f "$CONTAINER" >/dev/null 2>&1 && ok "容器已删除" || err "删除容器失败"
  fi
else
  info "第 1 步：容器不存在，跳过。"
fi

# ---------- 2) 删镜像 ----------
echo ""
if [ -n "$IMAGE" ]; then
  info "第 2 步：删除镜像 $IMAGE ..."
  if docker rmi "$IMAGE" >/dev/null 2>&1; then
    ok "镜像已删除"
  else
    warn "镜像删除失败（可能仍被其他容器占用），可稍后手动执行：docker rmi $IMAGE"
  fi
else
  info "第 2 步：无关联镜像，跳过。"
fi

# ---------- 3) 证书续期 ----------
echo ""
if [ -n "$ACME_DOMAINS" ]; then
  info "第 3 步：注销本项目证书的续期 ..."
  for d in $ACME_DOMAINS; do
    if "$ACME_BIN" --remove -d "$d" --ecc >/dev/null 2>&1; then
      ok "已注销 $d"
    else
      warn "注销 $d 失败，可手动执行：$ACME_BIN --remove -d $d --ecc"
    fi
  done
  # 没有其他证书了才移除定时任务，避免影响别人的续期
  LEFT=$("$ACME_BIN" --list 2>/dev/null | tail -n +2 | grep -c . || true)
  if [ "$LEFT" -le 0 ]; then
    if crontab -l 2>/dev/null | grep -q 'acme.sh'; then
      crontab -l 2>/dev/null | grep -v 'acme.sh' | crontab - 2>/dev/null \
        && ok "已移除 acme.sh 每日续期任务" || warn "移除续期任务失败，请手动编辑：crontab -e"
    fi
  else
    info "仍有 $LEFT 张其他证书，保留 acme.sh 续期任务不动。"
  fi
else
  info "第 3 步：未发现指向本目录的证书续期，跳过。"
fi

# ---------- 4) 删目录 ----------
echo ""
info "第 4 步：删除安装目录 ..."

rm_dir() {
  local d="$1"
  if rm -rf "$d" >/dev/null 2>&1; then
    return 0
  fi
  # 常见原因：data/ 由容器以 root 写入，普通用户删不掉
  if command -v sudo >/dev/null 2>&1; then
    if sudo -n true 2>/dev/null || [ -t 0 ]; then
      sudo rm -rf "$d" && return 0
    fi
  fi
  return 1
}

DIR_OK=0
if [ "$KEEP_DATA" = "1" ]; then
  # 先把 data 挪出来，删完目录再放回
  TMPD=$(mktemp -d)
  if [ -d "$DIR/data" ]; then
    cp -a "$DIR/data" "$TMPD/" 2>/dev/null \
      || { command -v sudo >/dev/null 2>&1 && sudo cp -a "$DIR/data" "$TMPD/" 2>/dev/null; }
  fi
  if rm_dir "$DIR"; then
    mkdir -p "$DIR"
    [ -d "$TMPD/data" ] && { cp -a "$TMPD/data" "$DIR/" 2>/dev/null \
      || { command -v sudo >/dev/null 2>&1 && sudo cp -a "$TMPD/data" "$DIR/" 2>/dev/null; }; }
    rm -rf "$TMPD" 2>/dev/null || true
    ok "目录已清空，data/ 已保留"
    DIR_OK=1
  fi
else
  if rm_dir "$DIR"; then
    ok "安装目录已删除"
    DIR_OK=1
  fi
fi

echo ""
if [ "$DIR_OK" != "1" ]; then
  echo "=============================================="
  err "卸载未完成：安装目录删除失败"
  echo "=============================================="
  echo "  容器、镜像、证书续期已按上面结果处理，但目录还在："
  echo "    $DIR"
  echo ""
  echo "  多半是 data/ 里的文件由容器以 root 写入，普通用户删不掉。"
  echo "  请手动执行："
  echo "    sudo rm -rf $DIR"
  echo ""
  exit 1
fi

echo "=============================================="
ok "卸载完成"
echo "=============================================="
echo "  cloud-mail 服务器上的邮件一封没动，网页版照常可用。"
echo "  若还要彻底停用，记得去路由器/安全组/DNS 关掉相关规则。"
echo "=============================================="
echo ""
