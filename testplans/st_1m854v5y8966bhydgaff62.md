# Test plan: st_1m854v5y8966bhydgaff62

CPU profile of the resize test on current main. Captures the sample distribution: expected to show ~1/3 in GC (gcBgMarkWorker) plus mallocgcLarge/memclrNoHeapPointers/bulkBarrierPreWrite/wbMove from the by-value model copies.

```sh produces=cpu.prof,pprof_top_cum.txt,pprof_top.txt,run.log inputs=internal/tui/resize_anchor_test.go,internal/tui/tui.go
set -e
go test -cpuprofile=$OUT/cpu.prof -run TestResize_OffsetStaysInRangeAcrossEveryHeightChange -count=1 ./internal/tui/... 2>&1 | tee $OUT/run.log
go tool pprof -top -cum $OUT/cpu.prof > $OUT/pprof_top_cum.txt
go tool pprof -top $OUT/cpu.prof > $OUT/pprof_top.txt
```
