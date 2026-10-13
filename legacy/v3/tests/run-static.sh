#!/usr/bin/env bash
set -euo pipefail
ROOT=$(cd -- "$(dirname -- "$0")/.." && pwd)
while IFS= read -r -d '' file; do bash -n "$file"; done < <(find "$ROOT" -type f -name '*.sh' -print0)
bash "$ROOT/tests/test-core.sh"
bash "$ROOT/tests/test-systemd.sh"
node --test "$ROOT/tests/settings.test.mjs"
if node -e 'require("ws")' >/dev/null 2>&1; then
    node --test "$ROOT/tests/bridge/bridge.test.mjs"
else
    printf '[SKIP] bridge tests: set NODE_PATH to an existing ws 8.21.3 parent or install test dependencies in a temporary directory\n'
fi
bash "$ROOT/addons/tests/test-addons.sh"
printf '[PASS] offline suite (see any SKIP above)\n'
