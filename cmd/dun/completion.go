package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/iodesystems/dun"
)

// `dun completion [bash|zsh|fish]` prints a completion script for
// `source <(dun completion bash)` (USER 2026-11-17). It completes the
// subcommands (cli.go), the flags of the real flag set (so it cannot drift
// from them) and, after --resume, the session ids --sessions lists, through
// the hidden `dun __complete sessions`. Both run before flag.Parse: no
// server, session or LLM starts.

// completionCmd runs `completion …` or `__complete …`; args start at that word.
func completionCmd(args []string, fs *flag.FlagSet, stdout, stderr io.Writer) int {
	if args[0] == "__complete" {
		return completeHelper(args[1:], stdout, stderr)
	}
	shell := ""
	switch len(args) {
	case 1:
		shell = filepath.Base(os.Getenv("SHELL"))
	case 2:
		shell = args[1]
	default:
		fmt.Fprintln(stderr, "usage: dun completion [bash|zsh|fish]")
		return 2
	}
	script, ok := completionScripts[shell]
	if !ok {
		if len(args) == 1 {
			fmt.Fprintf(stderr, "dun completion: cannot tell the shell from $SHELL (%q); name it: dun completion bash|zsh|fish\n", os.Getenv("SHELL"))
		} else {
			fmt.Fprintf(stderr, "dun completion: %q is not a shell dun completes; use bash, zsh or fish\n", shell)
		}
		return 2
	}
	io.WriteString(stdout, script(completionFlags(fs)))
	return 0
}

// completeHelper is `dun __complete sessions [--workspace DIR]`: the session
// ids, one a line, newest first, as --sessions prints them.
func completeHelper(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "sessions" {
		fmt.Fprintln(stderr, "usage: dun __complete sessions [--workspace DIR]")
		return 2
	}
	ws := "."
	if len(args) == 3 && args[1] == "--workspace" && args[2] != "" {
		ws = args[2]
	}
	abs, err := filepath.Abs(ws)
	if err != nil {
		return 1
	}
	for _, id := range dun.ListSessions(abs) {
		fmt.Fprintln(stdout, id)
	}
	return 0
}

// complFlag is one flag as a script completes it.
type complFlag struct {
	name  string // with its dashes
	help  string // the first clause of its usage, for fish
	value bool   // takes a value
}

// completionFlags are fs's flags less the removed ones, sorted, and --help.
func completionFlags(fs *flag.FlagSet) []complFlag {
	var out []complFlag
	fs.VisitAll(func(f *flag.Flag) {
		if removedFlags[f.Name] {
			return
		}
		dash := "--"
		if len(f.Name) == 1 {
			dash = "-"
		}
		bf, isBool := f.Value.(interface{ IsBoolFlag() bool })
		out = append(out, complFlag{name: dash + f.Name, help: firstClause(f.Usage), value: !(isBool && bf.IsBoolFlag())})
	})
	out = append(out, complFlag{name: "--help", help: "the flags and how to start"}, complFlag{name: "--usage", help: "what dun has used, and what is left"})
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

// firstClause is a usage text up to its first ':', ';' or '(' — short enough
// for a completion menu. A short "mode: " lead ("replay: ", "TUI: ") is
// dropped first: it is not the description.
func firstClause(s string) string {
	if i := strings.IndexByte(s, ':'); i > 0 && i < 12 && !strings.Contains(s[:i], " ") {
		s = s[i+1:]
	}
	if i := strings.IndexAny(s, ":;("); i > 0 {
		s = s[:i]
	}
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > 60 {
		s = string(r[:57]) + "..."
	}
	return s
}

func subcommandNames() string {
	var n []string
	for _, c := range subcommands {
		n = append(n, c.name)
	}
	return strings.Join(n, " ")
}

func flagNames(fl []complFlag) string {
	var n []string
	for _, f := range fl {
		n = append(n, f.name)
	}
	return strings.Join(n, " ")
}

var completionScripts = map[string]func([]complFlag) string{
	"bash": bashCompletion,
	"zsh":  zshCompletion,
	"fish": fishCompletion,
}

func bashCompletion(fl []complFlag) string {
	return `# dun completion for bash: source <(dun completion bash)
_dun() {
  local cur="${COMP_WORDS[COMP_CWORD]}" prev="${COMP_WORDS[COMP_CWORD-1]}" ws="" i
  for ((i = 1; i < COMP_CWORD; i++)); do
    [[ ${COMP_WORDS[i]} == --workspace ]] && ws="${COMP_WORDS[i+1]}"
  done
  if [[ $COMP_CWORD -eq 2 && ${COMP_WORDS[1]} == completion ]]; then
    COMPREPLY=($(compgen -W "bash zsh fish" -- "$cur"))
    return
  fi
  case "$prev" in
    --resume) COMPREPLY=($(compgen -W "$(dun __complete sessions --workspace "$ws" 2>/dev/null)" -- "$cur")); return ;;
    --workspace) COMPREPLY=($(compgen -d -- "$cur")); return ;;
    --media|--replay) COMPREPLY=($(compgen -f -- "$cur")); return ;;
  esac
  if [[ $COMP_CWORD -eq 1 && $cur != -* ]]; then
    COMPREPLY=($(compgen -W "` + subcommandNames() + `" -- "$cur"))
    return
  fi
  if [[ $cur == -* ]]; then
    COMPREPLY=($(compgen -W "` + flagNames(fl) + `" -- "$cur"))
  fi
}
complete -o default -F _dun dun
`
}

func zshCompletion(fl []complFlag) string {
	return `#compdef dun
# dun completion for zsh: source <(dun completion zsh)
_dun() {
  local -a subs flags
  local ws="" i=${words[(I)--workspace]}
  (( i > 0 && i < CURRENT - 1 )) && ws=${words[i+1]}
  if (( CURRENT == 3 )) && [[ ${words[2]} == completion ]]; then
    compadd -- bash zsh fish
    return
  fi
  subs=(` + subcommandNames() + `)
  flags=(` + flagNames(fl) + `)
  case ${words[CURRENT-1]} in
    --resume) compadd -- ${(f)"$(dun __complete sessions --workspace "$ws" 2>/dev/null)"}; return ;;
    --workspace) _files -/; return ;;
    --media|--replay) _files; return ;;
  esac
  if [[ $PREFIX == -* ]]; then
    compadd -- $flags
  elif (( CURRENT == 2 )); then
    compadd -- $subs
  else
    _default
  fi
}
if ! (( $+functions[compdef] )); then autoload -Uz compinit && compinit; fi
compdef _dun dun
`
}

func fishCompletion(fl []complFlag) string {
	q := func(s string) string {
		return "'" + strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), "'", `\'`) + "'"
	}
	var b strings.Builder
	b.WriteString(`# dun completion for fish: dun completion fish | source
function __dun_sessions
    set -l words (commandline -opc)
    set -l ws ""
    for i in (seq (count $words))
        if test "$words[$i]" = --workspace; and test $i -lt (count $words)
            set ws $words[(math $i + 1)]
        end
    end
    dun __complete sessions --workspace "$ws" 2>/dev/null
end
complete -c dun -f
complete -c dun -n '__fish_seen_subcommand_from completion' -a 'bash zsh fish'
`)
	for _, c := range subcommands {
		fmt.Fprintf(&b, "complete -c dun -n 'test (count (commandline -opc)) -eq 1' -a %s -d %s\n", c.name, q(c.help))
	}
	for _, f := range fl {
		opt := "-l " + strings.TrimPrefix(f.name, "--")
		if !strings.HasPrefix(f.name, "--") {
			opt = "-s " + strings.TrimPrefix(f.name, "-")
		}
		switch {
		case f.name == "--resume":
			opt += " -x -a '(__dun_sessions)'"
		case f.name == "--workspace":
			opt += " -x -a '(__fish_complete_directories)'"
		case f.name == "--media" || f.name == "--replay":
			opt += " -r -F"
		case f.value:
			opt += " -x"
		}
		fmt.Fprintf(&b, "complete -c dun %s -d %s\n", opt, q(f.help))
	}
	return b.String()
}
