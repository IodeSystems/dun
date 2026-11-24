# tuiModel size and resize-test baseline

Measured 2026-11-24 on `main@a4e952b27`, shared 24-core box (load varies).

## tuiModel size

```
unsafe.Sizeof(tuiModel{}) = 136,272 bytes
```

(136,120 bytes as measured 2026-11-18 in the issue; the 152-byte drift is
normal struct evolution.)

## Resize test wall time

`go test -run TestResize_OffsetStaysInRangeAcrossEveryHeightChange ./internal/tui/...`

Three runs, best = **10.6 s** (runs: 13.4 s, 13.2 s, 10.6 s).
Box load varies run-to-run; the 2026-11-18 figure in `test-times.md` was 15.63 s.

## CPU profile sample distribution

`go test -cpuprofile -run TestResize_OffsetStaysInRangeAcrossEveryHeightChange`
(12.96 s wall, 17.60 s total samples = 135.84%, 4+ active P's)

### GC (cumulative)

| function | cum | cum% |
|---|---|---|
| `runtime.gcBgMarkWorker` | 5.84 s | 33.18 % |
| `runtime.gcDrain` | 5.67 s | 32.22 % |
| `runtime.scanObject` | 2.81 s | 15.97 % |
| `runtime.scanSpan` | 1.39 s | 7.90 % |
| `runtime.markroot` | 1.22 s | 6.93 % |
| `runtime.scanblock` | 0.65 s | 3.69 % |
| `runtime.bgsweep` | 1.25 s | 7.10 % |

### Large-object copy / memclr machinery (cumulative)

| function | cum | cum% |
|---|---|---|
| `runtime.mallocgc` | 2.34 s | 13.30 % |
| `runtime.mallocgcLarge` | 2.10 s | 11.93 % |
| `runtime.memclrNoHeapPointers` | 1.63 s flat | 9.26 % |
| `runtime.bulkBarrierPreWrite` | 1.05 s | 5.97 % |
| `runtime.wbMove` | 0.98 s | 5.57 % |

### Hot TUI functions (cumulative)

| function | cum | cum% |
|---|---|---|
| `tuiModel.Update` | 9.14 s | 51.93 % |
| `tuiModel.update` | 4.75 s | 26.99 % |
| `(*tuiModel).refresh` | 2.87 s | 16.31 % |
| `tuiModel.convoHeightBudget` | 2.50 s | 14.20 % |
| `tuiModel.headView` | 0.76 s | 4.32 % |
| `(*tuiModel).renderEntry` | 0.69 s | 3.92 % |
| `tuiModel.paste` | 0.47 s | 2.67 % |

## What the final subtask reports against

After pointer-izing `mcpFields`, `providerFields` and `insp` (99,496 bytes
removed), re-measure:

1. `unsafe.Sizeof(tuiModel{})` — expected ~36,776 bytes.
2. Best-of-3 wall time of the resize test.
3. A fresh cpuprofile top, to show the GC + memclr + mallocgcLarge rows
   shrink in step with the smaller value copy.
