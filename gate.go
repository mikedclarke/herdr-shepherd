package main

import (
	"errors"
	"fmt"
	"time"
)

// defaultGateTimeoutMinutes bounds a gate that omits gate_timeout_minutes.
const defaultGateTimeoutMinutes = 10

type gateVerdict int

const (
	// gateRun: exit 0, the agent runs.
	gateRun gateVerdict = iota
	// gateSkip: exit 75 (the deferral code scripts use), the occurrence is
	// recorded skipped and no workspace opens.
	gateSkip
	// gateFailed: any other exit or a timeout kill; the agent runs anyway,
	// because a gate is a filter, not a dependency.
	gateFailed
)

// runGate runs the action's gate command in the action directory with
// SHEPHERD_ACTION and SHEPHERD_TRIGGER set, exactly as the pane would see
// them, and returns its verdict with the output tail.
func runGate(a *Action, trigger string) (gateVerdict, string) {
	out := &tailBuffer{max: outputTailMax}
	env := []string{"SHEPHERD_ACTION=" + a.Name, "SHEPHERD_TRIGGER=" + trigger}
	timeout := time.Duration(a.GateTimeoutMinutes) * time.Minute
	err := runCommandTracked(a.Name, a.Dir(), a.Gate, timeout, env, out, nil)
	switch {
	case err == nil:
		return gateRun, out.String()
	case isDeferExit(err):
		return gateSkip, out.String()
	case errors.Is(err, errScriptTimeout):
		return gateFailed, fmt.Sprintf("gate %v: %s", err, out.String())
	default:
		return gateFailed, fmt.Sprintf("gate failed (%v): %s", err, out.String())
	}
}

// skippedRecord is the run record a skipped occurrence leaves.
func skippedRecord(a *Action, detail, trigger string, began time.Time) runRecord {
	return runRecord{
		At: time.Now(), Action: a.Name, Kind: a.Kind, Status: "skipped", Detail: detail,
		Trigger: trigger, DurationSecs: durationSecs(began),
	}
}
