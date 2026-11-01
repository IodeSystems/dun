package main

import (
	"context"
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
	"github.com/iodesystems/agentkit/llm"

	"github.com/iodesystems/dun"
)

// dun spend: what has been spent, and the budget left. The ledger is the design.
// Anthropic reports exact tokens and no money, and reading its cost report takes full
// admin, which a corp account will not have and an individual account cannot have. So
// the spend is tokens times prices, worked out here and labelled as an estimate. The
// user can anchor it: dun spend reconcile <usd> takes the real figure off the Console,
// and only what came after it is estimated.
//
// The report is read too, when an admin credential happens to be there: a key file, or
// an `ant` login. That is a bonus, and nothing asks for it.
//
// A corrallm in front of Anthropic will answer this for its clients (its /v1/spend).

// spendKnown is a real figure and when it was true.
type spendKnown struct {
	USD float64   `json:"usd"`
	At  time.Time `json:"at"`
}

// spendKeyFile is where an Admin key goes. Beside the other oauth tokens, and not
// in config.json, because it can manage the whole org.
func spendKeyFile() string { return filepath.Join(dunHome(), "oauth", "anthropic-admin.key") }

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

// spendLine is what is known, in words: the report if this account can read it, else
// the ledger, from the last figure the user gave if it is from this month.
func spendLine(ctx context.Context, s anthropicadmin.Source, fc dunConfig, now time.Time) string {
	if s.How() != "" {
		if got, err := s.MonthToDate(ctx, now); err == nil {
			line := fmt.Sprintf("spend: $%.2f billed since %s (org-wide, usually within 5 minutes)", got.USD, got.Since.Format("Jan 2"))
			if fc.SpendBudget > 0 {
				line += fmt.Sprintf(" · $%.2f left of your $%.2f budget", fc.SpendBudget-got.USD, fc.SpendBudget)
			}
			return line
		}
	}
	y, m, _ := now.Date()
	known := fc.SpendKnown
	var line string
	var spent float64
	if ky, km, _ := known.At.Date(); ky == y && km == m {
		since := estimatedSince(known.At)
		spent = known.USD + since
		line = fmt.Sprintf("spend: ~$%.2f est this month: $%.2f from you on %s, then ~$%.2f est through this dun",
			spent, known.USD, known.At.Format("Jan 2 15:04"), since)
	} else {
		spent = estimatedThisMonth()
		line = fmt.Sprintf("spend: ~$%.2f est this month, through this dun only", spent)
	}
	if fc.SpendBudget > 0 {
		line += fmt.Sprintf(" · ~$%.2f est left of your $%.2f budget", fc.SpendBudget-spent, fc.SpendBudget)
	}
	return line
}

// estimatedThisMonth is the ledger's estimates this month, on every key.
var estimatedThisMonth = func() float64 {
	return dun.NewSpendLedger(dun.DefaultLedgerDir()).Summary("", time.Time{}).Month[llm.CurrencyUSDEstimate]
}

// estimatedSince is the ledger's estimates since t, counted from the start of its
// hour, on every key.
var estimatedSince = func(t time.Time) float64 {
	return dun.NewSpendLedger(dun.DefaultLedgerDir()).Summary("", t).Interval[llm.CurrencyUSDEstimate]
}

// setSpendKnown saves the real figure and says how far the ledger was from it.
func setSpendKnown(value string, now time.Time) (string, error) {
	n, err := strconv.ParseFloat(strings.TrimPrefix(value, "$"), 64)
	if err != nil || n < 0 {
		return "", fmt.Errorf("reconcile: the amount the Console shows this month, in USD")
	}
	fc := loadConfig()
	fc.SpendKnown = spendKnown{USD: n, At: now}
	if err := saveConfig(fc); err != nil {
		return "", err
	}
	return fmt.Sprintf("spend: $%.2f this month as of now (saved). This dun's ledger had ~$%.2f est", n, estimatedThisMonth()), nil
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
		return spendLine(ctx, spendSource(ctx), loadConfig(), time.Now())
	case len(f) == 2 && f[0] == "reconcile":
		msg, err := setSpendKnown(f[1], time.Now())
		if err != nil {
			return "spend: " + err.Error()
		}
		return msg
	case len(f) == 2 && f[0] == "budget":
		msg, err := setSpendBudget(f[1])
		if err != nil {
			return "spend: " + err.Error()
		}
		return msg
	case f[0] == "login":
		return "spend: in a terminal, run dun spend login"
	}
	return "usage: /spend [reconcile <usd> | budget <usd>|off]"
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
		fmt.Fprintln(stdout, spendLine(ctx, s, loadConfig(), time.Now()))
	case rest[0] == "reconcile" && len(rest) == 2:
		msg, err := setSpendKnown(rest[1], time.Now())
		if err != nil {
			fmt.Fprintln(stderr, "dun spend:", err)
			return 1
		}
		fmt.Fprintln(stdout, msg)
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
		fmt.Fprintln(stderr, "usage: dun spend [reconcile <usd> | budget <usd>|off | login [--no-browser] | logout]")
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
		fmt.Fprintln(stderr, "dun spend: only an organization account with admin rights can read billed spend, and dun does not need it.")
		fmt.Fprintln(stderr, "If yours can: install Anthropic's ant (go install github.com/anthropics/anthropic-cli/v2/cmd/ant@latest), or put an Admin key in", spendKeyFile(), "with mode 0600.")
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
