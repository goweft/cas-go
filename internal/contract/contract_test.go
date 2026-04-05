package contract_test

import (
	"testing"

	"github.com/goweft/cas/internal/contract"
)

func TestDefaultContractAllowedTypes(t *testing.T) {
	for _, wsType := range []string{"document", "code", "list"} {
		c := contract.DefaultWorkspaceContract(wsType, 1024)
		if err := c.CheckPreconditions(); err != nil {
			t.Errorf("type %q: unexpected precondition violation: %v", wsType, err)
		}
	}
}

func TestDefaultContractRejectsUnknownType(t *testing.T) {
	c := contract.DefaultWorkspaceContract("spreadsheet", 1024)
	if err := c.CheckPreconditions(); err == nil {
		t.Error("expected precondition violation for unknown type")
	}
}

func TestDefaultContractRejectsOversizedContent(t *testing.T) {
	c := contract.DefaultWorkspaceContract("document", 600*1024) // over 512KB
	if err := c.CheckPostconditions(); err == nil {
		t.Error("expected postcondition violation for oversized content")
	}
}

func TestViolationError(t *testing.T) {
	v := &contract.Violation{
		Agent: "cas-workspace", Phase: "precondition",
		Rule: "workspace_type_allowed", Detail: "got: spreadsheet",
	}
	s := v.Error()
	if s == "" {
		t.Error("expected non-empty error string")
	}
}
