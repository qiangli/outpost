# Evidence (Sprint 737 L2)

Gate run on the Windows gate host's WSL lane, 2026-11-27, through the standard
dag targets (`bashy dag -f dag.md cert|mutate|alone`), reference outpost built
from commit `791fd29dac401492acfe597875846073bc48d33e`.

| Target | Result | Wall |
|---|---|---|
| `cert` | PASS, 3 runs, none noisy (`cert.json`, contract sha256 `b14d8560…` = this CONTRACT.md) | 768 s |
| `mutate` | strength 20/20 (`mutate.log`) | 4211 s |
| `alone` | lone copy: source → build → test all ok (`alone-gate.log`) | 34 s |

The `dag: effect cap` line in `alone-gate.log` is a warning only (the cap
denies only through an in-body `@guard`).
