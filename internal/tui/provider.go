package tui

import (
	"fmt"
	"strings"
	"sync"

	tea "charm.land/bubbletea/v2"

	"github.com/iodesystems/dun/internal/dunconfig"
	"github.com/iodesystems/dun/internal/textfmt"
)

// /provider — which AI services this dun is logged in to, and which one the
// session runs on (USER 2026-11-04: "dun has very little ability to discover
// what provider you are on, what its models are; login and logout are not
// obvious").
//
//	/provider               every provider: in use or not, address, key,
//	                        how many models it lists, or why it lists none
//	/provider login [name]  /login: add a provider, or edit one
//	/provider logout <name> /logout
//
// Several providers can be logged in at once: /model picks among all of
// them, and a sub-agent can run on another one than its parent.

// providerStatus is one provider's answer to /v1/models.
type providerStatus struct {
	name, url string
	key       bool
	models    int
	err       string
}

// providerStatusMsg carries every provider's answer, in config order.
type providerStatusMsg struct{ rows []providerStatus }

func providerSlash(m *tuiModel, args []string) tea.Cmd {
	if len(args) > 0 {
		switch args[0] {
		case "login":
			return loginSlash(m, args[1:])
		case "logout":
			return logoutSlash(m, args[1:])
		}
		m.append(StErr.Render("usage: /provider [login [name] | logout <name>]"))
		return nil
	}
	ps := m.pickerProviders()
	if len(ps) == 0 {
		m.append(StDim.Render("no provider: /provider login to connect one"))
		return nil
	}
	m.append(StDim.Render("asking " + textfmt.Plural(len(ps), "provider") + " for their models…"))
	return func() tea.Msg {
		rows := make([]providerStatus, len(ps))
		var wg sync.WaitGroup
		for i, p := range ps {
			wg.Add(1)
			go func() {
				defer wg.Done()
				ids, why := dunconfig.ListModels(p.URL, p.Key)
				rows[i] = providerStatus{name: p.Name, url: p.URL, key: p.Key != "", models: len(ids), err: why}
			}()
		}
		wg.Wait()
		return providerStatusMsg{rows: rows}
	}
}

// providerListing renders /provider's answer. inUse is the session's
// endpoint and model.
func providerListing(rows []providerStatus, inURL, inModel string) string {
	var b strings.Builder
	b.WriteString(stHeader.Render("providers"))
	for _, r := range rows {
		mark, line := "  ", r.name
		if strings.TrimRight(r.url, "/") == strings.TrimRight(inURL, "/") && inURL != "" {
			mark = "● "
			if inModel != "" {
				line += " — in use: " + inModel
			}
		}
		key := "no key"
		if r.key {
			key = "key"
		}
		status := textfmt.Plural(r.models, "model")
		if r.models == 0 {
			status = "✗ " + r.err
			if r.err == "" {
				status = "✗ lists no models"
			}
		}
		fmt.Fprintf(&b, "\n%s%s\n    %s · %s · %s", mark, line, r.url, key, status)
	}
	b.WriteString("\n" + StDim.Render("/model picks a model from any of them · /provider login [name] adds or edits one · /provider logout <name>"))
	return b.String()
}
