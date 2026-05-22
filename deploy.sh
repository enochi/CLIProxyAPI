#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SKILL_SCRIPT="${SKILL_SCRIPT:-/home/zengym/.codex/skills/deploy-cliproxyapi-service/scripts/deploy-cliproxyapi-and-cpa-manager.sh}"

if [ ! -x "${SKILL_SCRIPT}" ]; then
  echo "ERROR: deploy helper not found or not executable: ${SKILL_SCRIPT}" >&2
  exit 1
fi

# Keep the repo root as the default source of truth when launched from this shortcut.
export CLI_REPO_DIR="${CLI_REPO_DIR:-${SCRIPT_DIR}}"

exec "${SKILL_SCRIPT}" "$@"
