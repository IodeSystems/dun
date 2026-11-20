package dundcli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/iodesystems/dun/proto"
)

// `dund enable|disable web|tui` (web.go).

func enableCmd(l *loader) *cobra.Command {
	c := &cobra.Command{Use: "enable", Short: "turn on the web listener (web) or the TUI in the browser (tui)"}
	var addr, publicURL string
	var trust []string
	var pair bool
	web := &cobra.Command{
		Use:   "web",
		Short: "serve the chooser, the web app (experimental) and the HTTP API",
		Long: "Starts dund's HTTP listener now, and at every later start (saved in\n" +
			"<home>/dund/web.json). Browsers sign in with a token (dund token) or the\n" +
			"identity provider. Default address " + defaultWebAddr + ".\n\n" +
			"--url names the public address a phone opens (a tunnel such as cloudflared,\n" +
			"a proxy); it is saved. --token then prints a QR code of a one-time link to\n" +
			"that address: scanned within 10 minutes, it signs that phone in, once. The\n" +
			"phone shows in `dun dund token list` as paired-<date>-via-qr-<hex>; revoke it\n" +
			"there.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ws := proto.WebSet{Enable: ptr(true), Addr: addr, PublicURL: publicURL, Pair: pair}
			if cmd.Flags().Changed("trust-proxy") {
				ws.TrustProxies = []string{} // --trust-proxy= alone clears the list
				for _, p := range trust {
					if p = strings.TrimSpace(p); p != "" {
						ws.TrustProxies = append(ws.TrustProxies, p)
					}
				}
			}
			return webSet(l, cmd.OutOrStdout(), ws)
		},
	}
	web.Flags().StringVar(&addr, "addr", "", "host:port to listen on (default: the saved one, else "+defaultWebAddr+")")
	web.Flags().StringVar(&publicURL, "url", "", "the public base URL a phone opens, e.g. https://name.trycloudflare.com (saved)")
	web.Flags().BoolVar(&pair, "token", false, "print a QR code of a one-time sign-in link to the public URL (10 minutes, one use)")
	web.Flags().StringArrayVar(&trust, "trust-proxy", nil, "a reverse proxy (CIDR or address) whose X-Forwarded-Proto/-Host to believe, besides loopback (repeatable; replaces the saved list; --trust-proxy= clears it)")
	tui := &cobra.Command{
		Use:   "tui",
		Short: "serve the dun TUI in the browser at /tui, on the web listener",
		Long: "Turns on /tui: each browser tab runs `dun --tui` in a pty, as this user, attached\n" +
			"to this dund. Behind the same sign-in as the web app. A separate opt-in from\n" +
			"`enable web`, which it needs.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return webSet(l, cmd.OutOrStdout(), proto.WebSet{TUI: ptr(true)})
		},
	}
	c.AddCommand(web, tui)
	return c
}

func disableCmd(l *loader) *cobra.Command {
	c := &cobra.Command{Use: "disable", Short: "turn off the web listener (web) or the TUI in the browser (tui)"}
	c.AddCommand(&cobra.Command{
		Use: "web", Short: "stop the HTTP listener, now and at later starts", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return webSet(l, cmd.OutOrStdout(), proto.WebSet{Enable: ptr(false)})
		},
	}, &cobra.Command{
		Use: "tui", Short: "stop serving /tui (its open tabs are closed)", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return webSet(l, cmd.OutOrStdout(), proto.WebSet{TUI: ptr(false)})
		},
	})
	return c
}

func ptr[T any](v T) *T { return &v }

// webSet sends ws to the running dund, or, when none answers, saves it for
// the next start.
func webSet(l *loader, out io.Writer, ws proto.WebSet) error {
	l.load()
	st, err := webSetLive(l.socket(), ws)
	if errors.Is(err, errNoDund) {
		if ws.Pair { // the code lives in dund's memory
			return errors.New("--token needs dund running (it keeps the code): dun dund serve, or dun dund service start")
		}
		return webSetOffline(l, out, ws)
	}
	if err != nil {
		return err
	}
	printWeb(out, st)
	if st.Pair != nil {
		return printPair(out, *st.Pair)
	}
	return nil
}

// printPair is the pairing link: its QR code, the link, and its expiry.
func printPair(out io.Writer, p proto.WebPair) error {
	qr, err := qrText(p.URL)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "\nScan with the phone's camera (one use, until %s):\n\n%s\n  %s\n", p.Expires.Local().Format("15:04:05"), qr, p.URL)
	if strings.HasPrefix(p.URL, "http://") {
		fmt.Fprintln(os.Stderr, "warning: the link is plain http: anyone on the path sees the code")
	}
	fmt.Fprintln(out, "  Anyone who opens this link first gets in. Do not share it.")
	return nil
}

var errNoDund = errors.New("no dund")

func webSetLive(sock string, ws proto.WebSet) (proto.WebState, error) {
	c, err := net.DialTimeout("unix", sock, 2*time.Second)
	if err != nil {
		return proto.WebState{}, errNoDund
	}
	defer c.Close()
	if err := json.NewEncoder(c).Encode(proto.NewCommand("web", &ws)); err != nil {
		return proto.WebState{}, err
	}
	_ = c.SetReadDeadline(time.Now().Add(15 * time.Second))
	dec := json.NewDecoder(c)
	for {
		var ev proto.Event
		if err := dec.Decode(&ev); err != nil {
			return proto.WebState{}, err
		}
		if ev.Re != "web" {
			continue
		}
		switch p := ev.Payload.(type) {
		case *proto.Ack:
			if p.Error != "" {
				return proto.WebState{}, errors.New(p.Error)
			}
		case *proto.WebState:
			return *p, nil
		}
	}
}

func webSetOffline(l *loader, out io.Writer, ws proto.WebSet) error {
	st, ok, err := loadWebState(l.home)
	if err != nil {
		return err
	}
	if !ok {
		// What the next start would seed from: the installed unit's flag.
		if a := unitWebAddr(l.home); a != "" {
			st = webState{Enabled: true, Addr: a}
		}
	}
	st, err = applyWebSet(st, ws)
	if err != nil {
		return err
	}
	if err := saveWebState(l.home, st); err != nil {
		return err
	}
	printWeb(out, wireState(st, false))
	fmt.Fprintf(out, "dund is not running: saved in %s; applies when dund next starts\n", webStatePath(l.home))
	return nil
}

func printWeb(out io.Writer, st proto.WebState) {
	tui := "off"
	if st.TUI {
		tui = "on (/tui)"
	}
	switch {
	case !st.Enabled:
		fmt.Fprintf(out, "web: off (address %s; tui %s)\n", st.Addr, tui)
		if st.TUI {
			fmt.Fprintln(out, "  the TUI is served on the web listener: dun dund enable web")
		}
	case st.Listening:
		fmt.Fprintf(out, "web: on at %s (tui %s)\n  open %s and sign in with a token (dun dund token)\n", st.Addr, tui, st.URL)
	default:
		fmt.Fprintf(out, "web: on at %s (tui %s)\n", st.Addr, tui)
	}
	if st.PublicURL != "" {
		fmt.Fprintf(out, "  public URL: %s\n", st.PublicURL)
	}
	if len(st.TrustProxies) > 0 {
		fmt.Fprintf(out, "  trusted proxies (besides loopback): %s\n", strings.Join(st.TrustProxies, ", "))
	}
	if st.Warning != "" {
		fmt.Fprintln(os.Stderr, "warning:", st.Warning)
	}
}
