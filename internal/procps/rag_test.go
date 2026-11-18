package procps

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/iodesystems/dun/proto"
)

// ragAt is the reading's time of ragFixture.
var ragAt = time.Unix(2_000_000_000, 0)

// ragFixture is dund's rag section on machine(): project a (session
// 20261117-093015 and a second session of the same project) ingesting its
// code and failing its sessions, a project no session works in, and two
// indexes idle for longer than RagIdleAfter.
func ragFixture() *proto.ProcRag {
	return &proto.ProcRag{
		Embed: proto.ProcRagEmbed{Model: "nomic-embed-text", Where: "local", InFlight: 2, Width: 4},
		Indexes: []proto.ProcRagIndex{
			{Name: "a/code", Project: "a", Kind: "code", State: "embedding", Queue: 12, Running: 2,
				EmbedInFlight: 2, FilesMin: 30, ChunksMin: 210, EmbedsMin: 8, Files: 400, IdleSec: 0},
			{Name: "a/docs", Project: "a", Kind: "docs", State: "idle", IdleSec: 300},
			{Name: "a/sessions", Project: "a", Kind: "sessions", State: "error", IdleSec: 90,
				LastErr: "embed: 503 overloaded", LastErrAt: ragAt.Add(-90 * time.Second)},
			{Name: "a/branches/wt-1a2b3c4d/code", Project: "a", Kind: "code", Branch: "wt-1a2b3c4d",
				State: "ingesting", Queue: 1000, QueueCapped: true, IdleSec: 1},
			{Name: "other/docs", Project: "other", Kind: "docs", State: "ingesting", Queue: 3, IdleSec: 4},
			{Name: "b/code", Project: "b", Kind: "code", State: "idle", IdleSec: 7200},
			{Name: "global/memories", Project: "", Kind: "memories", State: "idle", IdleSec: -1},
		},
	}
}

func ragMachine() (Snapshot, *proto.ProcsList, func(int) string) {
	snap, list, cwd := machine()
	list.Sessions[0].Project = "a"
	list.Sessions = append(list.Sessions, proto.ProcSession{ID: "20261117-100000", Workspace: "/w/a2", Project: "a"})
	list.Rag = ragFixture()
	return snap, list, cwd
}

func TestAttachRag_AttributesAndFolds(t *testing.T) {
	snap, list, cwd := ragMachine()
	rows := Attribute(snap, list, cwd)
	AttachRag(rows, list.DundPID, list.Rag, list.Sessions, ragAt)
	d := byPID(rows)[50]
	if d.RagEmbed == nil || d.RagEmbed.Model != "nomic-embed-text" {
		t.Fatalf("dund's embed: %+v", d.RagEmbed)
	}
	var active []string
	for _, ix := range d.Rag {
		active = append(active, ix.Name+"="+ix.Group)
	}
	want := "a/code=20261117-093015 a/docs=20261117-093015 a/sessions=20261117-093015 " +
		"a/branches/wt-1a2b3c4d/code=20261117-093015 other/docs=(none)"
	if got := strings.Join(active, " "); got != want {
		t.Errorf("active:\n got %s\nwant %s", got, want)
	}
	if len(d.RagIdle) != 2 || d.RagIdle[1].Group != GroupGlobal {
		t.Errorf("idle: %+v", d.RagIdle)
	}
	if d.Rag[0].Others != 1 || RagSession(d.Rag[0]) != "1117-0930 a +1" {
		t.Errorf("a second session of the project: %+v %q", d.Rag[0], RagSession(d.Rag[0]))
	}
	if d.Rag[2].ErrAgeSec != 90 {
		t.Errorf("error age: %+v", d.Rag[2])
	}
	// No rag section: nothing attached; another pid's row is untouched.
	rows = Attribute(snap, list, cwd)
	AttachRag(rows, list.DundPID, nil, list.Sessions, ragAt)
	AttachRag(rows, 999, list.Rag, list.Sessions, ragAt)
	if d := byPID(rows)[50]; d.RagEmbed != nil || d.Rag != nil {
		t.Errorf("rag with none: %+v", d)
	}
}

func TestRagTable(t *testing.T) {
	snap, list, cwd := ragMachine()
	rows := Attribute(snap, list, cwd)
	AttachRag(rows, list.DundPID, list.Rag, list.Sessions, ragAt)
	out := Table(Groups(rows), 0)
	for _, want := range []string{
		"  rag embed nomic-embed-text (local)  in flight 2  width 4",
		"  rag a code  embedding  queue 12  files/min 30  embeds/min 8",
		"  rag a code@wt-1a2b3c4d  ingesting  queue 1000+  files/min 0  embeds/min 0",
		"  rag a sessions  error  queue 0  files/min 0  embeds/min 0  err 1m embed: 503 overloaded",
		"  rag other docs  ingesting  queue 3",
		"  2 idle indexes",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("table lacks %q:\n%s", want, out)
		}
	}
	lines := strings.Split(out, "\n")
	dund := -1
	for i, ln := range lines {
		if strings.HasPrefix(ln, "50 ") {
			dund = i
		}
		if strings.Contains(ln, "rag a code ") && !strings.Contains(ln, "1117-0930 a +1") {
			t.Errorf("rag a code not under its sessions: %q", ln)
		}
		if strings.Contains(ln, "rag other docs") && !strings.Contains(ln, GroupNone) {
			t.Errorf("rag other docs has an owner: %q", ln)
		}
		if strings.Contains(ln, "rag b code") || strings.Contains(ln, "rag global") {
			t.Errorf("an idle index not folded: %q", ln)
		}
	}
	if dund < 0 || !strings.Contains(lines[dund+1], "rag embed") {
		t.Errorf("the rag lines are not under dund's row:\n%s", out)
	}
	// Cut to a terminal's width, no line is wider.
	for _, w := range []int{60, 100} {
		for _, ln := range strings.Split(Table(Groups(rows), w), "\n") {
			if n := len([]rune(ln)); n > w {
				t.Errorf("width %d: a line of %d: %q", w, n, ln)
			}
		}
	}
	// --json carries every index, the idle ones too.
	b, err := json.Marshal(Groups(rows))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"rag_embed":{"model":"nomic-embed-text"`, `"name":"a/code"`, `"rag_idle":[`, `"name":"global/memories"`, `"files_min":30`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("json lacks %s", want)
		}
	}
}

func TestRagEmbedLine(t *testing.T) {
	if got := RagEmbedLine(proto.ProcRagEmbed{}); got != "rag embed off (search by text only)  in flight 0" {
		t.Errorf("off: %q", got)
	}
	got := RagEmbedLine(proto.ProcRagEmbed{Model: "m", Where: "remote", InFlight: 1, Width: 2, N429: 3, BackedOff: true})
	if got != "rag embed m (remote)  in flight 1  width 2  429s 3  backed off" {
		t.Errorf("backed off: %q", got)
	}
}
