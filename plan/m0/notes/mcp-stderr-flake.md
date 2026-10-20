---
question: Why does TestStart_FailingAutostartIsNotFatal fail sometimes?
source: dep update 2026-10-19; agentkit mcpmgr/manager.go:380
---
Flaky ~2 in 6 runs. On a failed handshake agentkit calls mc.close() BEFORE
tail.settle(); close() closes the stderr pipe (jsonrpc.go:398), so the server's
own explanation can be lost and the error reads only "broken pipe". Fix belongs
in agentkit: settle before close. Not fixed as of 2026-10-20.
