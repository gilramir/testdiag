package inspect

import "sync"

// TokenUsage records token consumption from one or more LLM calls.
type TokenUsage struct {
	Prompt     int
	Completion int
	Total      int
}

// Add returns the element-wise sum of u and other.
func (u TokenUsage) Add(other TokenUsage) TokenUsage {
	return TokenUsage{
		Prompt:     u.Prompt + other.Prompt,
		Completion: u.Completion + other.Completion,
		Total:      u.Total + other.Total,
	}
}

// IsZero reports whether no tokens were recorded.
func (u TokenUsage) IsZero() bool { return u.Total == 0 }

var (
	usageMu      sync.Mutex
	currentUsage TokenUsage
)

// ResetUsage zeroes the package-level usage accumulator. Call before each
// stage run to scope collection to that stage.
func ResetUsage() {
	usageMu.Lock()
	currentUsage = TokenUsage{}
	usageMu.Unlock()
}

// addUsage adds u to the package-level accumulator. Called by httpClient.Chat
// after each successful completion.
func addUsage(u TokenUsage) {
	usageMu.Lock()
	currentUsage = currentUsage.Add(u)
	usageMu.Unlock()
}

// CollectUsage returns and resets the accumulated usage since the last
// ResetUsage call.
func CollectUsage() TokenUsage {
	usageMu.Lock()
	u := currentUsage
	currentUsage = TokenUsage{}
	usageMu.Unlock()
	return u
}
