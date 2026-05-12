package session

import (
	"testing"

	"github.com/fordjent/fordjent/internal/provider"
)

func TestBuildTurnSignature(t *testing.T) {
	calls := []provider.ToolCall{
		{Function: provider.FunctionCall{Name: "bash", Arguments: `{"command":"ls"}`}},
	}
	sig := buildTurnSignature(calls)
	if sig.tools == "" {
		t.Fatal("expected non-empty signature")
	}
}

func TestBuildTurnSignatureIdentical(t *testing.T) {
	calls := []provider.ToolCall{
		{Function: provider.FunctionCall{Name: "bash", Arguments: `{"command":"ls"}`}},
	}
	sig1 := buildTurnSignature(calls)
	sig2 := buildTurnSignature(calls)
	if sig1.tools != sig2.tools {
		t.Fatalf("identical calls should produce identical signatures: %q vs %q", sig1.tools, sig2.tools)
	}
}

func TestBuildTurnSignatureDifferent(t *testing.T) {
	sig1 := buildTurnSignature([]provider.ToolCall{
		{Function: provider.FunctionCall{Name: "bash", Arguments: `{"command":"ls"}`}},
	})
	sig2 := buildTurnSignature([]provider.ToolCall{
		{Function: provider.FunctionCall{Name: "read_file", Arguments: `{"path":"main.go"}`}},
	})
	if sig1.tools == sig2.tools {
		t.Fatal("different calls should produce different signatures")
	}
}

func TestAllSameSignature(t *testing.T) {
	sig := turnSignature{tools: "bash(abcd)"}
	if !allSameSignature([]turnSignature{sig, sig, sig}) {
		t.Fatal("expected same signatures to return true")
	}
	sig2 := turnSignature{tools: "read(efgh)"}
	if allSameSignature([]turnSignature{sig, sig, sig2}) {
		t.Fatal("expected mixed signatures to return false")
	}
}
