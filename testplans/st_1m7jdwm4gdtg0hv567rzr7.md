# Test plan: st_1m7jdwm4gdtg0hv567rzr7

The full harness package suite under reduced CPU, three times over: `go test . -count=3 -cpu=1 -timeout 30m` in internal/harness. Any test failing on any of the three passes fails the block. `suite.log` holds the full output (ok/FAIL per pass); `load.txt` is /proc/loadavg at start. This is the issue's own suggested proxy for machine load, since stress is not in the dev toolset.

```sh produces=suite.log,load.txt inputs=internal/harness/attach_test.go,internal/harness/servers_runtime.go
cat /proc/loadavg > $OUT/load.txt
go test ./internal/harness -count=3 -cpu=1 -timeout 30m > $OUT/suite.log 2>&1
status=$?
tail -3 $OUT/suite.log
exit $status
```

The two tests the issue names, each five times under reduced CPU: `go test ./internal/harness -count=5 -cpu=1 -run 'TestSetIndex_OwnIndexOffNarrowsTheSearch|TestStart_FailingAutostartIsNotFatal'`. `named.log` holds the verbose output; any FAIL on any of the five passes fails the block. These are the tests that flaked under the 2026-10-30 load average of 18-26 while passing 20/20 alone.

```sh produces=named.log inputs=internal/harness/attach_test.go,internal/harness/servers_runtime_test.go,internal/harness/servers_runtime.go
go test ./internal/harness -count=5 -cpu=1 -timeout 30m -run 'TestSetIndex_OwnIndexOffNarrowsTheSearch|TestStart_FailingAutostartIsNotFatal' -v > $OUT/named.log 2>&1
status=$?
grep -cE '^(=== RUN|--- PASS|--- FAIL)' $OUT/named.log | sed 's/^/test lines: /'
grep -E '^(ok|FAIL|--- FAIL)' $OUT/named.log
exit $status
```
