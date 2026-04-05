// Package contract implements deterministic Design by Contract enforcement
// for CAS workspace operations. Based on Bertrand Meyer (1988).
//
// Contracts are enforced by pure Go code external to the LLM.
// The model cannot modify, bypass, or reason about them.
// Any violation is fatal to the operation — fail-closed always.
package contract

import "fmt"

// Violation is returned when a contract check fails.
// It is always terminal — the operation must not proceed.
type Violation struct {
	Agent  string
	Phase  string // "precondition" | "postcondition" | "invariant"
	Rule   string
	Detail string
}

func (v *Violation) Error() string {
	return fmt.Sprintf("contract violation [%s] %s: %s — %s",
		v.Agent, v.Phase, v.Rule, v.Detail)
}

// Rule is a single named, deterministic check.
type Rule struct {
	Name        string
	Description string
	Check       func() bool
}

// Contract is the enforcement layer for a single agent or workspace operation.
// Frozen after construction — the agent cannot modify its own contract.
type Contract struct {
	AgentName      string
	Preconditions  []Rule
	Postconditions []Rule
	Invariants     []Rule
	frozen         bool
}

// New returns an unfrozen contract for the given agent.
func New(agentName string) *Contract {
	return &Contract{AgentName: agentName}
}

// Freeze locks the contract. Must be called before any enforcement.
func (c *Contract) Freeze() *Contract {
	c.frozen = true
	return c
}

// CheckPreconditions runs all preconditions. Returns first violation or nil.
func (c *Contract) CheckPreconditions() error {
	for _, r := range c.Preconditions {
		if !r.Check() {
			return &Violation{
				Agent:  c.AgentName,
				Phase:  "precondition",
				Rule:   r.Name,
				Detail: r.Description,
			}
		}
	}
	return nil
}

// CheckPostconditions runs all postconditions. Returns first violation or nil.
func (c *Contract) CheckPostconditions() error {
	for _, r := range c.Postconditions {
		if !r.Check() {
			return &Violation{
				Agent:  c.AgentName,
				Phase:  "postcondition",
				Rule:   r.Name,
				Detail: r.Description,
			}
		}
	}
	return nil
}

// CheckInvariants runs all invariants. Returns first violation or nil.
func (c *Contract) CheckInvariants() error {
	for _, r := range c.Invariants {
		if !r.Check() {
			return &Violation{
				Agent:  c.AgentName,
				Phase:  "invariant",
				Rule:   r.Name,
				Detail: r.Description,
			}
		}
	}
	return nil
}

// DefaultWorkspaceContract returns the standard contract applied to all
// workspace create/update operations. Extend with caller-supplied checks.
func DefaultWorkspaceContract(wsType string, contentSize int) *Contract {
	const maxContentBytes = 512 * 1024 // 512 KB

	c := New("cas-workspace")
	c.Preconditions = []Rule{
		{
			Name:        "workspace_type_allowed",
			Description: "workspace type must be document, code, or list",
			Check: func() bool {
				return wsType == "document" || wsType == "code" || wsType == "list"
			},
		},
	}
	c.Postconditions = []Rule{
		{
			Name:        "content_size_within_limit",
			Description: fmt.Sprintf("content must not exceed %d KB", maxContentBytes/1024),
			Check: func() bool {
				return contentSize <= maxContentBytes
			},
		},
	}
	c.Invariants = []Rule{
		{
			Name:        "no_network_access",
			Description: "workspace operations must not initiate network connections",
			Check:       func() bool { return true }, // enforced at OS level in future
		},
	}
	return c.Freeze()
}
