// Package failmode describes how the failing test behaves across runs and
// supplies the mode-specific framing that every diagnosis stage injects into its
// prompts.
//
// There are two modes:
//
//   - Flaky (the default): the test passes on most runs and failed only
//     intermittently. The root cause is almost always some source of
//     NONDETERMINISM — a race, ordering assumption, timeout, resource limit, or
//     environmental condition.
//   - AlwaysFails (operator passed --always-fails): the test fails on EVERY run.
//     The root cause is almost always a DETERMINISTIC defect — a real bug, a bad
//     assertion, a contract/schema mismatch, a missing fixture, an
//     environment/version change, or a recently introduced regression.
//
// Steering the analyst toward the right class of cause matters: a flaky-mode
// prompt that insists "this must be a race" actively misleads when the test is a
// consistent regression, and vice versa.
package failmode

// Mode describes the failure behavior of the test under investigation. The zero
// value is the default: a flaky (intermittent) test.
type Mode struct {
	// AlwaysFails is true when the operator passed --always-fails, meaning the
	// test fails deterministically on every run rather than intermittently.
	AlwaysFails bool
}

// Flaky returns the default mode (intermittent failure).
func Flaky() Mode { return Mode{AlwaysFails: false} }

// ShortLabel is a terse noun phrase for the failure, suitable inline in prose.
func (m Mode) ShortLabel() string {
	if m.AlwaysFails {
		return "consistently failing test"
	}
	return "flaky test"
}

// Description states, authoritatively, how the test behaves. Stages inject it
// near the top of their system prompt so the model never has to guess.
func (m Mode) Description() string {
	if m.AlwaysFails {
		return "This automated test FAILS ON EVERY RUN — it is NOT flaky and NOT intermittent. The same failure reproduces deterministically on every execution."
	}
	return "This automated test is FLAKY — it passes on most runs and failed only intermittently on this run."
}

// CausePrior tells the analyst which class of root cause to favor, and which to
// distrust, given the failure mode.
func (m Mode) CausePrior() string {
	if m.AlwaysFails {
		return "Because it fails every single run, the cause is almost certainly DETERMINISTIC: a genuine logic bug, a broken or outdated assertion, an API/contract or schema mismatch, a missing dependency or fixture, an environment/version change, or a recently introduced regression. A race or timing window is UNLIKELY — those would make the test flaky, not consistently failing. If your explanation would predict the test sometimes passing, keep looking."
	}
	return "Because the test passes on most runs, the cause is almost never \"the code is simply wrong\" (that would fail every run). It is some source of NONDETERMINISM: a race, ordering assumption, timeout, resource limit, or environmental condition. If your explanation would predict the test always failing, keep looking."
}

// ConditionGuidance phrases what a hypothesis / brief should describe as the
// failure condition. Used by LOGPARSE and HYPOTHESIZE.
func (m Mode) ConditionGuidance() string {
	if m.AlwaysFails {
		return "the specific DETERMINISTIC defect (logic error, bad or outdated assertion, contract/schema mismatch, missing fixture or dependency, environment/version change, or recent regression) — not a race or timing window, since the test fails on every run"
	}
	return "a specific nondeterministic condition: a race, ordering assumption, timing window, timeout/deadline, resource or port collision, leftover state, or environmental variation"
}

// MechanismLabel describes what the DEEPINSPECT "## Mechanism" section must name.
func (m Mode) MechanismLabel() string {
	if m.AlwaysFails {
		return "The specific deterministic defect (logic bug / bad assertion / contract or schema mismatch / missing fixture / environment / regression) — or why none applies if REFUTED."
	}
	return "The specific nondeterministic condition (race / timing / ordering / resource / environment) — or why none applies if REFUTED."
}

// FeedbackConditionCriterion phrases the feedback-gate criterion that checks a
// stage output names a plausible failure condition of the right class.
func (m Mode) FeedbackConditionCriterion() string {
	if m.AlwaysFails {
		return "describes a plausible DETERMINISTIC defect (logic bug, bad assertion, contract/schema mismatch, missing fixture, environment/version change, or regression) consistent with a test that fails on every run — not a race or timing window"
	}
	return "describes a plausible nondeterministic condition (race, timing, ordering, resource, environment) — not just \"the code might be wrong\""
}
