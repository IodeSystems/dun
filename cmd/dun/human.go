package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/iodesystems/dun/internal/harness"
	"github.com/iodesystems/dun/internal/numfmt"
	"github.com/iodesystems/dun/internal/textfmt"
)

// bareQuietJobEnd is how long every remaining job must stay quiet before a
// one-shot run ends: a job that has printed nothing for this long is treated
// as one that will never print, and the run returns its answer instead of
// blocking until the outer --timeout.
var bareQuietJobEnd = 30 * time.Second

// runHuman streams a single task, then drains any background jobs it started
// (their completion notifications trigger follow-up turns).
func runHuman(ctx context.Context, h *harness.Harness, task, workspace string, imgs []harness.ImageInput, noTools bool) (reply string) {
	h.WaitServers(ctx) // a one-shot task has every tool it was started with
	if noTools {
		fmt.Fprintln(os.Stderr, "dun: no tools (--no-tools): a plain chat")
	} else {
		fmt.Fprintf(os.Stderr, "dun: %d tools ready: %s\n",
			len(h.ToolNames()), strings.Join(h.ToolNames(), ", "))
	}
	// Name what is NOT running. A one-shot run has no /rag to type, so point at
	// the flags instead of the commands.
	for _, st := range h.Servers() {
		switch {
		case st.Running:
		case st.Missing:
			fmt.Fprintf(os.Stderr, "dun: %s off: %s\n", st.ID, textfmt.OneLine(st.Err))
		case st.Err != "":
			fmt.Fprintf(os.Stderr, "dun: %s did not start: %s\n", st.ID, textfmt.OneLine(st.Err))
		default:
			fmt.Fprintf(os.Stderr, "dun: %s off (--%s to start it this run)\n", st.ID, flagOf(st.ID))
		}
	}
	fmt.Fprintf(os.Stderr, "\ntask: %s\n\n", task)
	if why := h.ImagesRefused(); len(imgs) > 0 && why != "" {
		fatal(errors.New(why))
	}
	res, err := h.AskImages(ctx, task, imgs)
	if err != nil {
		// The conversation is on disk, so this is recoverable — say how, since the
		// retries already visible above have run out and the alternative is a user
		// starting the whole task again.
		fmt.Fprintf(os.Stderr, "\ndun: resume with: dun --continue --workspace %s -p \"<what to do next>\"\n", workspace)
		fatal(turnErr(ctx, err))
	}
	fmt.Fprint(os.Stderr, numfmt.Commaf("\n\n--- done (%d tokens) ---\n", res.Usage.Total))
	reply = res.Reply

	for {
		if h.BackgroundRunning() == 0 {
			select {
			case <-h.Wake(): // a just-finished job's notification
			default:
				return
			}
		} else if h.AllJobsQuiet(bareQuietJobEnd) {
			// Every job has been quiet for the threshold: nothing is going to
			// wake us, so return the answer instead of blocking until --timeout.
			fmt.Fprintf(os.Stderr, "\n--- all jobs quiet; ending one-shot run ---\n")
			return
		} else {
			// Wait for a wake, a ctx cancel, or the quiet threshold to elapse
			// (whichever comes first). The timer is re-evaluated each loop
			// iteration; QuietIn shrinks as output stops, so the first tick
			// that fires is the one that makes AllJobsQuiet true.
			select {
			case <-h.Wake():
			case <-ctx.Done():
				return
			case <-time.After(h.QuietIn(bareQuietJobEnd)):
			}
		}
		fmt.Fprintf(os.Stderr, "\n--- background job finished; continuing ---\n")
		r, err := h.Continue(ctx)
		if err != nil {
			return
		}
		if strings.TrimSpace(r.Reply) != "" {
			reply = r.Reply
		}
	}
}

// humanAsk prompts on the terminal and reads a line. A number picks an option;
// with multi, comma-separated numbers (e.g. "1,3") pick several. When noCustom
// is set, free text is refused — the answer must be one of the numbered
// options.
func humanAsk(_ context.Context, question string, options []string, multi, noCustom bool, recommended []string) (string, error) {
	fmt.Fprintf(os.Stderr, "\n❓ %s\n", safeErr(question))
	for i, o := range options {
		mark := ""
		if slices.Contains(recommended, o) {
			mark = "  (recommended)"
		}
		fmt.Fprintf(os.Stderr, "   %d) %s%s\n", i+1, safeErr(o), mark)
	}
	if multi && len(options) > 0 {
		fmt.Fprint(os.Stderr, "answer (comma-separated numbers for several): ")
	} else if noCustom {
		fmt.Fprint(os.Stderr, "answer (number only): ")
	} else {
		fmt.Fprint(os.Stderr, "answer: ")
	}
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	line = strings.TrimSpace(line)
	if multi && len(options) > 0 && strings.ContainsAny(line, ", ") {
		var picked []string
		for _, f := range strings.FieldsFunc(line, func(r rune) bool { return r == ',' || r == ' ' }) {
			if n, err := strconv.Atoi(f); err == nil && n >= 1 && n <= len(options) {
				picked = append(picked, options[n-1])
			}
		}
		if len(picked) > 0 {
			return strings.Join(picked, ", "), nil
		}
	}
	if n, err := strconv.Atoi(line); err == nil && n >= 1 && n <= len(options) {
		return options[n-1], nil
	}
	if noCustom {
		return "", fmt.Errorf("answer must be one of the %d options (a number 1–%d)", len(options), len(options))
	}
	return line, nil
}
