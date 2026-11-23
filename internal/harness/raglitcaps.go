package harness

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// Raglit's read selector has no token inside a list that means "the default
// set" (own indexes plus the config's shared namespaces): "all" expands
// only as the WHOLE selector (nsReadSelector, raglit namespace.go), so a
// list of its own drops the shared namespaces. Dun works around it by
// reading raglit's config and adding each shared namespace back one at a
// time as "ns:*" (attachSelector).
//
// A raglit that supports the default-set token inside a list says so in its
// --help, and dun sends the token in place of "<own>:*" — it expands to
// own plus shared, so the per-namespace additions are no longer needed.
// The marker and the token are raglit's contract: they are pinned here
// against the raglit release that ships them (the pinned raglit, v0.10.0,
// has neither, so the gate reads false and the behavior is unchanged).

// raglitDefaultSetMarker is in the --help of a raglit whose read selector
// accepts the default-set token inside a list. A substring, as daemonMarker
// is: the help line names it, and a longer line still carries it.
const raglitDefaultSetMarker = "--index default-set"

// raglitDefaultSetToken is what a raglit that carries raglitDefaultSetMarker
// resolves to the default read set (own indexes plus the config's shared
// namespaces) where "<own>:*" would.
const raglitDefaultSetToken = "default-set"

// raglitCaps is what a raglit binary's read selector takes.
type raglitCaps struct {
	DefaultSet bool // the default-set token is usable inside a list
}

// raglitProbes caches raglitCaps by the binary's path, size and mtime: a
// rebuilt binary is asked again, an unchanged one is not — the IndexProtocol
// cache (indexProbes), for the raglit CLI.
var raglitProbes sync.Map // key → raglitCaps

// raglitHelp is the help text of `bin --help`: raglit's CLI prints its flags
// on --help (there is no --help-all in the raglit this pins), so one run
// covers every marker. A read.
func raglitHelp(bin string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), indexProbeTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "--help").CombinedOutput()
	if len(out) == 0 && err != nil {
		return "", fmt.Errorf("%s --help: %w", bin, err)
	}
	return string(out), nil
}

// RaglitCaps reports what the raglit named by command (a path, or a name
// looked up on PATH) takes of the read-selector protocol, read from its
// --help the way IndexProtocol reads poly-lsp-mcp's. resolved is the
// binary that was asked. A binary that cannot be found or run reports
// nothing: the gate stays closed and the search selector is built as before
// (own's indexes and the shared namespaces, one by one).
func RaglitCaps(command string) (sup raglitCaps, resolved string, err error) {
	resolved, err = exec.LookPath(command)
	if err != nil {
		return sup, "", err
	}
	if abs, err := filepath.Abs(resolved); err == nil {
		resolved = abs
	}
	st, err := os.Stat(resolved)
	if err != nil {
		return sup, resolved, err
	}
	key := fmt.Sprintf("raglit|%s|%d|%d", resolved, st.Size(), st.ModTime().UnixNano())
	if v, hit := raglitProbes.Load(key); hit {
		return v.(raglitCaps), resolved, nil
	}
	help, err := raglitHelp(resolved)
	if err != nil {
		log.Printf("dun: %s: raglit caps not known: %v", command, err)
		return sup, resolved, err
	}
	sup.DefaultSet = strings.Contains(help, raglitDefaultSetMarker)
	raglitProbes.Store(key, sup)
	return sup, resolved, nil
}
