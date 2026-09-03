#!/usr/bin/env bash
# uninstall-desktop.sh - Remove GNOME/XDG .desktop entry for Antigravity 2.0
set -euo pipefail

DESKTOP_DIR="${HOME}/.local/share/applications"
DESKTOP_FILE="${DESKTOP_DIR}/antigravity.desktop"

echo "==> Removing Antigravity desktop entry..."
if [ -f "${DESKTOP_FILE}" ]; then
    rm -f "${DESKTOP_FILE}"
    echo "Removed: ${DESKTOP_FILE}"
else
    echo "Desktop file not found: ${DESKTOP_FILE}"
fi

if command -v update-desktop-database >/dev/null 2>&1; then
    update-desktop-database "${DESKTOP_DIR}" || true
fi

echo "==> Desktop integration successfully uninstalled."
