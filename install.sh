#!/usr/bin/env bash
# Install the latest port_fwd release for the current platform.
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/Ken-u/port_forward/main/install.sh | bash
# Optional env:
#   PORT_FWD_INSTALL_DIR   install directory (default: ~/.local/bin)
#   PORT_FWD_VERSION       release tag like v0.1.5 (default: latest)
#   PORT_FWD_HOST          systemd listen host (default: 0.0.0.0)
#   PORT_FWD_PORT          systemd listen port (default: 9000)
#   PORT_FWD_SYSTEMD       yes/no — install user systemd service on Linux (default: ask)

set -euo pipefail

REPO="Ken-u/port_forward"
BIN_NAME="port_fwd"
DEFAULT_INSTALL_DIR="${HOME}/.local/bin"
INSTALL_DIR="${PORT_FWD_INSTALL_DIR:-$DEFAULT_INSTALL_DIR}"
VERSION="${PORT_FWD_VERSION:-}"
SERVICE_HOST="${PORT_FWD_HOST:-0.0.0.0}"
SERVICE_PORT="${PORT_FWD_PORT:-9000}"
SYSTEMD_CHOICE="${PORT_FWD_SYSTEMD:-}"

info() { printf '==> %s\n' "$*"; }
warn() { printf 'warning: %s\n' "$*" >&2; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }

need_cmd() {
  command -v "$1" >/dev/null 2>&1 || die "需要命令: $1"
}

detect_platform() {
  local os arch
  os="$(uname -s | tr '[:upper:]' '[:lower:]')"
  arch="$(uname -m)"

  case "$os" in
    linux) OS="linux" ;;
    darwin) OS="darwin" ;;
    *) die "暂不支持的操作系统: $(uname -s)。Windows 请手动下载 Release zip。" ;;
  esac

  case "$arch" in
    x86_64|amd64) ARCH="amd64" ;;
    aarch64|arm64) ARCH="arm64" ;;
    i386|i686|x86) ARCH="386" ;;
    *) die "暂不支持的 CPU 架构: $arch" ;;
  esac

  if [[ "$OS" == "darwin" && "$ARCH" == "386" ]]; then
    die "macOS 不支持 386 架构"
  fi

  ASSET="port_fwd-${OS}-${ARCH}.tar.gz"
  PLATFORM="${OS}-${ARCH}"
}

latest_tag() {
  local api_url json tag
  api_url="https://api.github.com/repos/${REPO}/releases/latest"
  if command -v curl >/dev/null 2>&1; then
    json="$(curl -fsSL "$api_url")"
  else
    json="$(wget -qO- "$api_url")"
  fi
  tag="$(printf '%s' "$json" | sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -n 1)"
  [[ -n "$tag" ]] || die "无法获取最新 Release 版本"
  printf '%s\n' "$tag"
}

download() {
  local url="$1" out="$2"
  if command -v curl >/dev/null 2>&1; then
    curl -fL --progress-bar -o "$out" "$url"
  else
    wget -O "$out" "$url"
  fi
}

ask_yes_no() {
  local prompt="$1" default="${2:-n}" reply
  if [[ -n "$SYSTEMD_CHOICE" ]]; then
    case "$(printf '%s' "$SYSTEMD_CHOICE" | tr '[:upper:]' '[:lower:]')" in
      y|yes|1|true) return 0 ;;
      n|no|0|false) return 1 ;;
      *) die "PORT_FWD_SYSTEMD 只能是 yes/no" ;;
    esac
  fi
  if [[ ! -t 0 ]]; then
    # Piped install (curl | bash): default to no unless env is set.
    return 1
  fi
  if [[ "$default" == "y" ]]; then
    prompt="${prompt} [Y/n] "
  else
    prompt="${prompt} [y/N] "
  fi
  read -r -p "$prompt" reply || true
  reply="$(printf '%s' "${reply:-$default}" | tr '[:upper:]' '[:lower:]')"
  [[ "$reply" == "y" || "$reply" == "yes" ]]
}

install_binary() {
  local tmpdir archive url binpath
  tmpdir="$(mktemp -d)"
  archive="${tmpdir}/${ASSET}"
  url="https://github.com/${REPO}/releases/download/${VERSION}/${ASSET}"

  info "下载 ${VERSION} / ${ASSET}"
  if ! download "$url" "$archive"; then
    rm -rf "$tmpdir"
    die "下载失败: $url"
  fi

  info "解压到临时目录"
  if ! tar -xzf "$archive" -C "$tmpdir"; then
    rm -rf "$tmpdir"
    die "解压失败: $ASSET"
  fi

  binpath="$(find "$tmpdir" -type f -name "port_fwd-${PLATFORM}" | head -n 1)"
  if [[ -z "$binpath" ]]; then
    rm -rf "$tmpdir"
    die "压缩包中未找到 port_fwd-${PLATFORM}"
  fi

  mkdir -p "$INSTALL_DIR"
  install -m 755 "$binpath" "${INSTALL_DIR}/${BIN_NAME}"
  rm -rf "$tmpdir"
  info "已安装: ${INSTALL_DIR}/${BIN_NAME}"

  if ! command -v "$BIN_NAME" >/dev/null 2>&1; then
    warn "当前 PATH 中还找不到 ${BIN_NAME}"
    warn "请把下面这行加到 shell 配置文件后重新打开终端："
    printf '  export PATH="%s:$PATH"\n' "$INSTALL_DIR"
  fi
}

write_user_service() {
  local unit_dir unit_file exec_bin
  exec_bin="${INSTALL_DIR}/${BIN_NAME}"
  unit_dir="${HOME}/.config/systemd/user"
  unit_file="${unit_dir}/port_fwd.service"

  mkdir -p "$unit_dir"
  cat >"$unit_file" <<EOF
[Unit]
Description=port_fwd TCP multiplex forwarder
Documentation=https://github.com/${REPO}
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=${exec_bin} --host ${SERVICE_HOST} --port ${SERVICE_PORT}
Restart=on-failure
RestartSec=3
WorkingDirectory=%h

[Install]
WantedBy=default.target
EOF

  info "已写入用户服务: ${unit_file}"
  info "启动参数: --host ${SERVICE_HOST} --port ${SERVICE_PORT}"

  systemctl --user daemon-reload
  systemctl --user enable --now port_fwd.service
  info "已启用并启动: systemctl --user status port_fwd"

  if command -v loginctl >/dev/null 2>&1; then
    if ! loginctl show-user "$USER" -p Linger 2>/dev/null | grep -q 'Linger=yes'; then
      warn "当前用户未开启 lingering。注销后用户服务可能停止。"
      warn "如需开机自启（即使未登录），可执行: loginctl enable-linger $USER"
    fi
  fi

  cat <<EOF

常用命令:
  systemctl --user status port_fwd
  systemctl --user restart port_fwd
  systemctl --user stop port_fwd
  journalctl --user -u port_fwd -f

管理页: http://127.0.0.1:${SERVICE_PORT}
EOF
}

maybe_install_systemd() {
  [[ "$OS" == "linux" ]] || return 0

  if ! command -v systemctl >/dev/null 2>&1; then
    warn "未检测到 systemctl，跳过 systemd 安装"
    return 0
  fi
  if ! systemctl --user show-environment >/dev/null 2>&1; then
    warn "当前环境无法使用用户级 systemd，跳过服务安装"
    return 0
  fi

  if ask_yes_no "是否安装到当前用户的 systemd 服务并开机自启？" "n"; then
    if [[ -t 0 && -z "${PORT_FWD_HOST:-}" ]]; then
      read -r -p "监听地址 [${SERVICE_HOST}]: " input_host || true
      SERVICE_HOST="${input_host:-$SERVICE_HOST}"
    fi
    if [[ -t 0 && -z "${PORT_FWD_PORT:-}" ]]; then
      read -r -p "监听端口 [${SERVICE_PORT}]: " input_port || true
      SERVICE_PORT="${input_port:-$SERVICE_PORT}"
    fi
    if ! [[ "$SERVICE_PORT" =~ ^[0-9]+$ ]] || (( SERVICE_PORT < 1 || SERVICE_PORT > 65535 )); then
      die "无效端口: $SERVICE_PORT"
    fi
    write_user_service
  else
    info "已跳过 systemd 服务安装"
    cat <<EOF

以后可手动启动:
  ${INSTALL_DIR}/${BIN_NAME} --port ${SERVICE_PORT}
EOF
  fi
}

main() {
  need_cmd uname
  need_cmd tar
  need_cmd install
  need_cmd mktemp
  need_cmd find
  if ! command -v curl >/dev/null 2>&1 && ! command -v wget >/dev/null 2>&1; then
    die "需要 curl 或 wget"
  fi

  detect_platform
  if [[ -z "$VERSION" ]]; then
    VERSION="$(latest_tag)"
  fi
  [[ "$VERSION" == v* ]] || VERSION="v${VERSION}"

  info "平台: ${PLATFORM}"
  info "版本: ${VERSION}"
  info "安装目录: ${INSTALL_DIR}"

  install_binary
  maybe_install_systemd

  info "完成。运行: ${BIN_NAME} --help"
}

main "$@"
