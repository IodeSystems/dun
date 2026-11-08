package dundcli

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/iodesystems/agentkit/llm"
	"github.com/spf13/cobra"

	"github.com/iodesystems/dun/internal/dunconfig"
	"github.com/iodesystems/dun/internal/ragd"
	"github.com/iodesystems/dun/proto"
)

// ragCmd is `dund rag …`: retrieval's socket commands by hand (docs/rag.md).
func ragCmd(l *loader) *cobra.Command {
	var project, index string
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "rag",
		Short: "search and fill dund's retrieval indexes",
		Long: "Indexes: a project (--project <slug>) has code, docs, memories and strategies;\n" +
			"the global set (no --project) has memories and strategies.",
	}
	cmd.PersistentFlags().StringVar(&project, "project", "", "the project's slug (default: the global set)")
	cmd.PersistentFlags().StringVar(&index, "index", "", "code, docs, memories or strategies (required but by probe)")
	cmd.PersistentFlags().BoolVar(&asJSON, "json", false, "print the reply as JSON")
	needIndex := func() error {
		if index == "" {
			return errors.New(`required flag "index" not set`)
		}
		return nil
	}

	show := func(v any, text func() error) error {
		if asJSON {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(v)
		}
		return text()
	}

	var mode, prefix string
	var limit int
	var tags []string
	var fields map[string]string
	search := &cobra.Command{
		Use:   "search QUERY...",
		Short: "search an index",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if err := needIndex(); err != nil {
				return err
			}
			l.load()
			var out proto.RagHits
			q := &proto.RagSearch{Project: project, Index: index, Query: strings.Join(args, " "), Mode: mode, Limit: limit,
				Filter: proto.RagFilter{PathPrefix: prefix, Tags: tags, Fields: fields}}
			if err := ragCall(l, q, &out); err != nil {
				return err
			}
			return show(out, func() error {
				if out.Note != "" {
					fmt.Fprintln(os.Stderr, "note:", out.Note)
				}
				if len(out.Hits) == 0 {
					fmt.Fprintln(os.Stderr, "no hits")
				}
				for _, h := range out.Hits {
					where := h.Path
					if h.Page > 0 {
						where += fmt.Sprintf(" p%d", h.Page)
					}
					fmt.Printf("%.4g  %s  %s\n    %s\n", h.Score, where, h.Title, oneLine(h.Text, 200))
				}
				return nil
			})
		},
	}
	search.Flags().StringVar(&mode, "mode", "", "hybrid (default) or fts")
	search.Flags().IntVar(&limit, "limit", 10, "most hits")
	search.Flags().StringVar(&prefix, "path-prefix", "", "only documents whose path (source URL, file:///…) starts with this")
	search.Flags().StringArrayVar(&tags, "tag", nil, "only documents with this tag (repeatable: all must hold)")
	search.Flags().StringToStringVar(&fields, "field", nil, "only documents whose field k equals v (k=v, repeatable)")

	var title string
	var itags []string
	ingest := &cobra.Command{
		Use:   "ingest PATH|URL",
		Short: "queue a file or URL for an index (the index is made on first use)",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if err := needIndex(); err != nil {
				return err
			}
			l.load()
			q := &proto.RagIngest{Project: project, Index: index, Title: title, Tags: itags}
			if strings.Contains(args[0], "://") {
				q.URL = args[0]
			} else {
				abs, err := filepath.Abs(args[0])
				if err != nil {
					return err
				}
				q.Path = abs
			}
			var out proto.RagIngested
			if err := ragCall(l, q, &out); err != nil {
				return err
			}
			return show(out, func() error {
				fmt.Printf("job %d: %s\n", out.Job, out.URL)
				return nil
			})
		},
	}
	ingest.Flags().StringVar(&title, "title", "", "the document's title")
	ingest.Flags().StringArrayVar(&itags, "tag", nil, "a tag to write with the document (repeatable)")

	var state string
	var jlimit int
	jobs := &cobra.Command{
		Use:   "jobs",
		Short: "list an index's ingest jobs, newest first",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if err := needIndex(); err != nil {
				return err
			}
			l.load()
			var out proto.RagJobList
			if err := ragCall(l, &proto.RagJobs{Project: project, Index: index, State: state, Limit: jlimit}, &out); err != nil {
				return err
			}
			return show(out, func() error {
				w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
				fmt.Fprintln(w, "ID\tSTATE\tFRAGMENTS\tURL\tERROR")
				for _, j := range out.Jobs {
					fmt.Fprintf(w, "%d\t%s\t%d\t%s\t%s\n", j.ID, j.State, j.Fragments, j.URL, oneLine(j.Error, 80))
				}
				return w.Flush()
			})
		},
	}
	jobs.Flags().StringVar(&state, "state", "", "pending, running, done or error (default: all)")
	jobs.Flags().IntVar(&jlimit, "limit", 20, "most jobs")

	reembed := &cobra.Command{
		Use:   "reembed",
		Short: "rebuild an index's vectors with the configured embed model",
		Long: "An index records the embed model and dimension its vectors came from. When the\n" +
			"embed role (facets.embed) differs, dund searches the index by text only and writes\n" +
			"no vectors to it. reembed drops the old vectors, records the new model, and embeds\n" +
			"every fragment again in the background (dund's log says when it is done).",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if err := needIndex(); err != nil {
				return err
			}
			l.load()
			var out proto.RagReembedding
			if err := ragCall(l, &proto.RagReembed{Project: project, Index: index}, &out); err != nil {
				return err
			}
			return show(out, func() error {
				what := "kept its vectors"
				if out.Dropped {
					what = "dropped its old vectors"
				}
				fmt.Printf("%s: %s; embed model %s%s; %d fragments to embed\n", out.Index, what, out.Model, dimSaid(out.Dim), out.Missing)
				return nil
			})
		},
	}

	probe := &cobra.Command{
		Use:   "probe [" + strings.Join(ragd.Facets, "|") + "]",
		Short: "probe the retrieval model roles (all, or one) and record the results",
		Long: "Each role's probe spends a few requests on its model and writes what it measured\n" +
			"to the capability record (<home>/capabilities, the record /model probe writes):\n" +
			"  embed     dimension, largest batch of 1/8/32/64, latency per batch, same vector twice\n" +
			"  vision    character error rate reading a generated image of known text\n" +
			"  segment   raglit's segmenter on a fixture document: valid fragments or why not\n" +
			"  identity  the chat probes of /model probe\n" +
			"Runs here, not in dund: the roles come from <home>/config.json's facets.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			l.load()
			facet := ""
			if len(args) == 1 {
				facet = args[0]
			}
			cfg := dunconfig.LoadConfigFile(filepath.Join(l.home, "config.json"))
			res, err := ragd.Probe(c.Context(), cfg, filepath.Join(l.home, "capabilities"), facet)
			if err != nil {
				return err
			}
			failed := false
			for i, r := range res {
				if r.Err != nil {
					failed = true
				}
				if asJSON {
					continue
				}
				if i > 0 {
					fmt.Println()
				}
				fmt.Println(r.Report())
			}
			if asJSON {
				type row struct {
					Facet string           `json:"facet"`
					Said  string           `json:"said"`
					Caps  llm.Capabilities `json:"caps,omitzero"`
					Error string           `json:"error,omitempty"`
				}
				var rows []row
				for _, r := range res {
					x := row{Facet: r.Facet, Said: r.Said, Caps: r.Caps}
					if r.Err != nil {
						x.Error = r.Err.Error()
					}
					rows = append(rows, x)
				}
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				if err := enc.Encode(rows); err != nil {
					return err
				}
			}
			if failed {
				return errors.New("a probe stopped")
			}
			return nil
		},
	}

	cmd.AddCommand(search, ingest, jobs, reembed, probe)
	return cmd
}

// ragCall sends payload to dund and decodes its reply into out (a pointer
// to the reply's payload type).
func ragCall(l *loader, payload any, out any) error {
	c, err := net.DialTimeout("unix", l.socket(), 2*time.Second)
	if err != nil {
		return fmt.Errorf("no service at %s (dund serve): %w", l.socket(), err)
	}
	defer c.Close()
	if err := json.NewEncoder(c).Encode(proto.NewCommand("rag", payload)); err != nil {
		return err
	}
	// A search may wait on the embed model; a reply is still bounded.
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Minute))
	dec := json.NewDecoder(c)
	for {
		var ev proto.Event
		if err := dec.Decode(&ev); err != nil {
			return err
		}
		if ev.Re != "rag" {
			continue
		}
		if a, ok := ev.Payload.(*proto.Ack); ok {
			if a.Error != "" {
				return errors.New(a.Error)
			}
			continue
		}
		b, err := json.Marshal(ev.Payload)
		if err != nil {
			return err
		}
		return json.Unmarshal(b, out)
	}
}

func dimSaid(n int) string {
	if n <= 0 {
		return ""
	}
	return fmt.Sprintf(" (dimension %d)", n)
}

// oneLine is s on one line, cut to n runes.
func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}
