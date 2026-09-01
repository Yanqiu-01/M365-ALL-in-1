package web

import (
	"log"
)

// routerOutcome records only the routing control-flow result. It deliberately
// excludes prompts, tool schemas, tool names, arguments, account identifiers,
// and model output so stage telemetry can be enabled during a live incident.
type routerOutcome struct {
	requestID     string
	routerStage   string
	declaredTools int
	choice        string
	intentLikely  bool
	parsed        bool
	rawCandidates int
	postLedger    int
	valid         int
	rejected      int
}

func newRouterOutcome(requestID, routerStage string, declaredTools int, choice any, intentLikely bool) routerOutcome {
	return routerOutcome{
		requestID:     requestID,
		routerStage:   routerStage,
		declaredTools: declaredTools,
		choice:        normalizedToolChoiceMode(choice),
		intentLikely:  intentLikely,
	}
}

func (o *routerOutcome) observeParsed(parsed bool, rawCandidates int) {
	o.parsed = parsed
	o.rawCandidates = rawCandidates
}

func (o *routerOutcome) observeValidated(postLedger, valid, rejected int) {
	o.postLedger = postLedger
	o.valid = valid
	o.rejected = rejected
}

func (o routerOutcome) fields(substage, disposition string) map[string]any {
	return map[string]any{
		"router_stage":    o.routerStage,
		"router_substage": substage,
		"disposition":     disposition,
		"declared_tools":  o.declaredTools,
		"tool_choice":     o.choice,
		"parsed":          o.parsed,
		"raw_candidates":  o.rawCandidates,
		"post_ledger":     o.postLedger,
		"valid_calls":     o.valid,
		"rejected_calls":  o.rejected,
		"tool_intent":     o.intentLikely,
	}
}

func (o routerOutcome) record(substage, disposition string) {
	fields := o.fields(substage, disposition)
	stage(o.requestID, "router_outcome", fields)
	log.Printf("[router-outcome] id=%s stage=%s substage=%s disposition=%s declared_tools=%d choice=%s parsed=%t raw_candidates=%d post_ledger=%d valid_calls=%d rejected_calls=%d tool_intent=%t",
		o.requestID, o.routerStage, substage, disposition, o.declaredTools, o.choice,
		o.parsed, o.rawCandidates, o.postLedger, o.valid, o.rejected, o.intentLikely)
}
