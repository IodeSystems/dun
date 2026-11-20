# Test plan: st_1m7j5vpcz989mby2bt0wcx

Per-model quality ceiling (F11): the shape target becomes `min(window - reserve, Q)`, with Q set in `/context config --ceiling`.

- `TestContextBudget_CeilingBoundsTheTarget`: a 1M window with a 96k ceiling shapes to 96k; a small window keeps `window - reserve`; a ceiling above the window's share cannot raise it. This is the "prompt reaches `min(window - reserve, Q)` → shaping triggers" clause, expressed at the budget the Shaper's ladder works to.
- `TestContextBudget_CeilingFromConfig`: `ConfigureContext(ceiling=64000)` moves the budget in force to 64k, `/context` status reports it, and `default` clears it — the "set in /context config" clause end to end.

Elision before fold is the Shaper's own ladder order (agentkit `shaper.go`: LOD/Rehead run before the fold); the ceiling changes the budget number, not the order, so no reordering test belongs in dun.

```sh produces=out.log inputs=internal/harness/shaping.go,internal/harness/window.go,internal/harness/compaction_test.go
go test ./internal/harness/... -run 'TestContextBudget_Ceiling|TestContextBudget_LeavesRoomForTheResponse' -v > $OUT/out.log 2>&1
```
