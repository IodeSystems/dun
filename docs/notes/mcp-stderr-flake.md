---
question: Why does TestStart_FailingAutostartIsNotFatal fail sometimes?
source: dep update 2026-10-19; agentkit mcpmgr/manager.go:380
---
Flaky ~2 in 6 runs. On a failed handshake agentkit called mc.close() BEFORE
tail.settle(); close() closes the stderr pipe (jsonrpc.go:398), so the server's
own explanation can be lost and the error reads only "broken pipe". Fix belongs
in agentkit: settle before close.

Settled. The reordering landed as agentkit commit b30c4066 "fix(mcpmgr): read a
refused server's stderr before closing its pipe", on agentkit main at
mcpmgr/manager.go:456-457 (`tail.settle()` then `mc.close()`), and it is
verified directly in the version dun pins: go.mod pins agentkit v0.41.2
(go.sum:110), and the v0.41.2 module cache source shows the same
settle-before-close order in the failed-handshake path
(manager.go:472-473, comment "Settle BEFORE close: close shuts the stderr
pipe...").
