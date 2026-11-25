package acp

import (
	"slices"

	sdk "github.com/coder/acp-go-sdk"
)

// caps is what the editor said it can take (initialize's
// clientCapabilities), as far as the bridge sends differently for it. An
// editor that says nothing gets ACP's baseline.
type caps struct {
	// form: elicitation/create in form mode (clientCapabilities.elicitation
	// .form): dun's questions are forms (elicit.go).
	form bool
	// planUpdate: the unstable plan_update session update
	// (clientCapabilities.plan; JetBrains Air's planContentDelta or
	// planFile): the plan is sent with it instead of plan.
	planUpdate bool
}

func readCaps(c sdk.ClientCapabilities) caps {
	k := caps{
		form:       c.Elicitation != nil && c.Elicitation.Form != nil,
		planUpdate: c.Plan != nil,
	}
	air := airCaps(c.Meta)
	if slices.Contains(air, "planContentDelta") || slices.Contains(air, "planFile") {
		k.planUpdate = true
	}
	return k
}

// airCaps is the list JetBrains Air advertises in initialize's _meta:
// {"jetbrains": {"air": {"version": 1, "capabilities": [...]}}} (seen in an
// Air debug bundle, 2026-11-24).
func airCaps(m sdk.Meta) []string {
	var jb struct {
		Air struct {
			Capabilities []string `json:"capabilities"`
		} `json:"air"`
	}
	if m == nil || m.Get("jetbrains", &jb) != nil {
		return nil
	}
	return jb.Air.Capabilities
}

// planID names a session's one plan in plan_update: per session, so an
// editor with several sessions keeps their plans apart.
func planID(session string) string { return "dun-" + session }

// planUpdate is dun's plan as the update the editor takes.
func planUpdate(k caps, session string, entries []sdk.PlanEntry) sdk.SessionUpdate {
	if !k.planUpdate {
		return sdk.UpdatePlan(entries...)
	}
	return sdk.SessionUpdate{PlanUpdate: &sdk.SessionPlanUpdate{Plan: sdk.NewPlanUpdateContentItems(sdk.PlanId(planID(session)), entries)}}
}
