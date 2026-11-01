package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/iodesystems/agentkit/anthropicadmin"

	"github.com/iodesystems/dun"
)

// dun spend: what Anthropic says was spent, and the budget left. Replies carry
// tokens and no money, so everything dun shows per turn is an estimate; this is
// the real figure, from the org's cost report. That report takes full admin on a
// Console org, so the flows are the user's to choose, by what is installed: a key
// file if there is one, else an `ant` login, which leaves dun no key to keep.
//
// A corrallm in front of Anthropic will answer this for its clients (its /v1/spend,
// with a token that reads spend and nothing else). Direct is for those without one.

// spendKeyFile is where an Admin key goes. Beside the other oauth tokens, and not
// in config.json, because it can manage the whole org.
func spendKeyFile() string { return filepath.Join(dunHome(), "oauth", "anthropic-admin.key") }

// usesAnthropic reports whether any provider is Anthropic's own endpoint.
func usesAnthropic(fc dunConfig) bool {
	if strings.Contains(fc.URL, "api.anthropic.com") {
		return true
	}
	for _, p := range fc.Providers {
		if strings.Contains(p.URL, "api.anthropic.com") {
			return true
		}
	}
	return false
}

// antPath is Anthropic's ant, "" when there is none. Apache Ant has the same name.
func antPath(ctx context.Context) string {
	p, err := dun.FindCommand("ant")
	if err != nil || !anthropicadmin.IsAnthropicAnt(ctx, p) {
		return ""
	}
	return p
}

func spendSource(ctx context.Context) anthropicadmin.Source {
	return anthropicadmin.Source{KeyFile: spendKeyFile(), Ant: antPath(ctx)}
}

// spendLine is what is known, in words.
func spendLine(ctx context.Context, s anthropicadmin.Source, budget float64, now time.Time) string {
	if s.How() == "" {
		return "spend: estimates only: no admin credential. dun spend login, after dun doctor --fix, or an Admin key in " + spendKeyFile()
	}
	got, err := s.MonthToDate(ctx, now)
	if errors.Is(err, anthropicadmin.ErrNoCredential) {
		return "spend: estimates only: not logged in. Run dun spend login"
	}
	if err != nil {
		return "spend: could not read the cost report: " + err.Error()
	}
	line := fmt.Sprintf("spend: $%.2f billed since %s (org-wide, usually within 5 minutes)", got.USD, got.Since.Format("Jan 2"))
	if budget > 0 {
		line += fmt.Sprintf(" · $%.2f left of your $%.2f budget", budget-got.USD, budget)
	}
	return line
}

// setSpendBudget saves action's budget and says so.
func setSpendBudget(value string) (string, error) {
	fc := loadConfig()
	if value == "off" {
		fc.SpendBudget = 0
	} else {
		n, err := strconv.ParseFloat(strings.TrimPrefix(value, "$"), 64)
		if err != nil || n <= 0 {
			return "", fmt.Errorf("budget: an amount in USD, or off")
		}
		fc.SpendBudget = n
	}
	if err := saveConfig(fc); err != nil {
		return "", err
	}
	if fc.SpendBudget == 0 {
		return "spend: no budget (saved)", nil
	}
	return fmt.Sprintf("spend: budget $%.2f a month (saved)", fc.SpendBudget), nil
}

// spendControl answers /spend. The login needs a terminal, which an engine does
// not have, so it gives the command.
func spendControl(ctx context.Context, action string) string {
	f := strings.Fields(strings.ToLower(action))
	switch {
	case len(f) == 0:
		ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		return spendLine(ctx, spendSource(ctx), loadConfig().SpendBudget, time.Now())
	case len(f) == 2 && f[0] == "budget":
		msg, err := setSpendBudget(f[1])
		if err != nil {
			return "spend: " + err.Error()
		}
		return msg
	case f[0] == "login":
		return "spend: in a terminal, run dun spend login"
	}
	return "usage: /spend [budget <usd>|off]"
}

// spendCmd is `dun spend`.
func spendCmd(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("dun spend", flag.ContinueOnError)
	fs.SetOutput(stderr)
	noBrowser := fs.Bool("no-browser", false, "login: print a URL and read a pasted code")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	ctx := context.Background()
	s := spendSource(ctx)
	switch rest := fs.Args(); {
	case len(rest) == 0:
		fmt.Fprintln(stdout, spendLine(ctx, s, loadConfig().SpendBudget, time.Now()))
	case rest[0] == "login" && len(rest) == 1:
		return spendLogin(s, *noBrowser, stdin, stdout, stderr)
	case rest[0] == "logout" && len(rest) == 1:
		if s.Ant == "" {
			fmt.Fprintln(stdout, "spend: ant is not installed; an Admin key is removed by deleting", spendKeyFile())
			return 0
		}
		return runAnt(s, s.LogoutArgs(), stdin, stdout, stderr)
	case rest[0] == "budget" && len(rest) == 2:
		msg, err := setSpendBudget(rest[1])
		if err != nil {
			fmt.Fprintln(stderr, "dun spend:", err)
			return 1
		}
		fmt.Fprintln(stdout, msg)
	default:
		fmt.Fprintln(stderr, "usage: dun spend [login [--no-browser] | logout | budget <usd>|off]")
		return 2
	}
	return 0
}

// spendLogin takes whichever flow is there.
func spendLogin(s anthropicadmin.Source, noBrowser bool, stdin io.Reader, stdout, stderr io.Writer) int {
	switch {
	case s.How() == "key":
		fmt.Fprintln(stdout, "spend: already set up, with the Admin key in", spendKeyFile())
		return 0
	case s.Ant == "":
		fmt.Fprintln(stderr, "dun spend: ant is not installed. Run dun doctor --fix, then dun spend login.")
		fmt.Fprintln(stderr, "Or create an Admin key in the Console and put it in", spendKeyFile(), "with mode 0600.")
		return 1
	}
	if _, err := os.Stat(s.Ant); err != nil {
		fmt.Fprintln(stderr, "dun spend:", err)
		return 1
	}
	return runAnt(s, s.LoginArgs(noBrowser), stdin, stdout, stderr)
}

func runAnt(s anthropicadmin.Source, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	cmd := exec.Command(s.Ant, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintln(stderr, "dun spend:", err)
		return 1
	}
	return 0
}

// antChecks is doctor's look at ant: that it is Anthropic's, and whether there is
// a way to real spend.
func (d *doctor) antChecks(p string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if !anthropicadmin.IsAnthropicAnt(ctx, p) {
		d.add("warn", "tool", "ant: "+p+" is Apache Ant, not Anthropic's", "put Anthropic's ant beside dun: "+dun.ToolInstall["ant"].Module)
	}
}
