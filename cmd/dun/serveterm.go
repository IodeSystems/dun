package main

import (
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"

	"github.com/iodesystems/dun/internal/tui"
	"github.com/iodesystems/dun/internal/webterm"
)

// dun --web-tui: the TUI in the browser (internal/webterm). Each browser gets
// its own `dun --tui` in a pty; use --continue to resume. dund serves the same
// page at /tui (`dun dund enable tui`), behind its sign-in; this flag is the
// older, unauthenticated way, kept until it is removed.
func runServe(o tui.TuiOpts, addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	for _, u := range reachableURLs(ln.Addr().String()) {
		log.Printf("dun --web-tui → %s", u)
	}
	if !loopbackOnly(addr) {
		log.Print("⚠ bound to a non-loopback address — no auth; anyone who can reach it can drive this agent")
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	// ctrl+c/esc do not quit: you leave by closing the tab, which drops the
	// socket and kills the process group. The session lives on in dund.
	o.DisableExit = true
	h := webterm.New(webterm.Config{
		Bin:  exe,
		Args: tui.ProcArgs(o, "--tui"),
		Env:  append(webEnv(os.Environ()), "DUN_CHILD=1"), // the spawned TUI never self-rebuilds
	})
	go watchShutdown(h)
	return http.Serve(ln, h)
}

// watchShutdown warns before Ctrl-C drops attached browser sessions; a second
// Ctrl-C confirms.
func watchShutdown(h *webterm.Handler) {
	sig := make(chan os.Signal, 2)
	signal.Notify(sig, os.Interrupt)
	<-sig
	if n := h.Active(); n > 0 {
		log.Printf("⚠ %d web session(s) attached — Ctrl-C again to drop them", n)
		<-sig
	}
	h.Close()
	os.Exit(0)
}

// reachableURLs turns a bound address into the URL(s) to open. For an all-
// interfaces bind (0.0.0.0 / :port / [::]) it lists the loopback plus each LAN
// IPv4, so a browser on another host knows where to point.
func reachableURLs(addr string) []string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return []string{"http://" + addr}
	}
	if host != "" && host != "0.0.0.0" && host != "::" {
		return []string{"http://" + net.JoinHostPort(host, port)}
	}
	urls := []string{"http://127.0.0.1:" + port}
	ifaces, _ := net.Interfaces()
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 || virtualIface(ifc.Name) {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok {
				if ip := ipn.IP.To4(); ip != nil && !ip.IsLoopback() {
					urls = append(urls, "http://"+ip.String()+":"+port)
				}
			}
		}
	}
	return urls
}

// virtualIface skips docker/bridge/vpn/virtual NICs, so the URL list is the
// real LAN address(es) a browser on another host would use — not a wall of
// 172.x bridge IPs.
func virtualIface(name string) bool {
	for _, p := range []string{"docker", "br-", "veth", "virbr", "tap", "tun", "cni", "flannel", "kube", "vmnet", "utun", "zt"} {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// loopbackOnly reports whether addr binds only the loopback interface.
func loopbackOnly(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// webEnv is the environment for a web session's TUI (webterm.CleanEnv).
func webEnv(env []string) []string { return webterm.CleanEnv(env) }
