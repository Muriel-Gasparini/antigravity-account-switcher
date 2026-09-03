#!/usr/bin/env bash
# ==============================================================================
# install-desktop.sh
# Installs Antigravity 2.0 application entry into GNOME / XDG desktop
# ==============================================================================
set -eo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

SWITCHER_BIN="${SWITCHER_BIN:-}"
if [[ -z "${SWITCHER_BIN}" ]]; then
  if [[ -x "${PROJECT_ROOT}/bin/antigravity-account-switcher" ]]; then
    SWITCHER_BIN="${PROJECT_ROOT}/bin/antigravity-account-switcher"
  elif command -v antigravity-account-switcher >/dev/null 2>&1; then
    SWITCHER_BIN="$(command -v antigravity-account-switcher)"
  elif [[ -x "${HOME}/.local/bin/antigravity-account-switcher" ]]; then
    SWITCHER_BIN="${HOME}/.local/bin/antigravity-account-switcher"
  else
    echo "Building antigravity-account-switcher..."
    make -C "${PROJECT_ROOT}" build
    SWITCHER_BIN="${PROJECT_ROOT}/bin/antigravity-account-switcher"
  fi
fi

"${SWITCHER_BIN}" install-desktop "$@"
