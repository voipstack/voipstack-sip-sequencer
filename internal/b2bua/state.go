// Package b2bua implements a SIP back-to-back user agent that sequences calls
// through external SIP application servers before terminating to a PBX.
package b2bua

import "github.com/voipstack/voipstack-sip-sequencer/internal/config"

// CallState tracks the lifecycle of a call.
type CallState string

const (
	stateSetup       CallState = "setup"
	stateEstablished CallState = "established"
	stateTearingDown CallState = "tearingDown"
)

// LegRole identifies which outbound leg a session represents.
type LegRole string

const (
	roleApplication LegRole = "application"
	rolePBX         LegRole = "pbx"
)

type failureKind int

const (
	failureReject  failureKind = iota // remote returned a non-2xx final response
	failureTimeout                    // no final response or transport error
)

// mapFailureStatus returns the SIP status code to send to the endpoint on leg failure.
// For a rejection the remote status is passed through; for timeout/transport 503 is used.
func mapFailureStatus(kind failureKind, appStatus int) int {
	if kind == failureReject {
		return appStatus
	}
	return 503
}

// canTransition reports whether moving from → to is a legal call-state move.
func canTransition(from, to CallState) bool {
	switch from {
	case stateSetup:
		return to == stateEstablished || to == stateTearingDown
	case stateEstablished:
		return to == stateTearingDown
	}
	return false
}

type failAction int

const (
	actionSkip failAction = iota
	actionAbort
)

// failureAction maps an application's OnFailure policy to the bridge action.
func failureAction(p config.FailurePolicy) failAction {
	if p == config.FailureAbort {
		return actionAbort
	}
	return actionSkip
}
