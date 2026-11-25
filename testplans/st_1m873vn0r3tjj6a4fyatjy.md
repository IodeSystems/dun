# Test plan: st_1m873vn0r3tjj6a4fyatjy

Mobile upgrade.test.ts: the new `failed`-state test passes alongside the existing pending/restarting/snapshot tests.

```sh produces=jest-upgrade.log
set -euo pipefail
cd mobile
npx jest test/upgrade.test.ts --no-cache 2>&1 | tee "$OUT/jest-upgrade.log"
```

Install mobile deps and run upgrade.test.ts to confirm the failed-state change.

```sh produces=pnpm-install.log,jest-upgrade.log
set -euo pipefail
cd mobile
# Install dependencies (pnpm, as the project uses).
pnpm install --frozen-lockfile 2>&1 | tail -5 | tee "$OUT/pnpm-install.log"
# Run only the upgrade test.
npx jest test/upgrade.test.ts --no-cache 2>&1 | tee "$OUT/jest-upgrade.log"
```

Full mobile jest suite: no regressions from the upgrade.ts change.

```sh produces=jest-full.log
set -euo pipefail
cd mobile
# Full mobile jest suite: no regressions.
npx jest --no-cache 2>&1 | tee "$OUT/jest-full.log"
```
