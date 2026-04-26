package main

import (
	"strings"
	"testing"
)

func TestReserveScriptContainsAtomicDebit(t *testing.T) {
	if !strings.Contains(reserveScript, `DECRBY`) {
		t.Fatal("reserve script should atomically debit balance")
	}
	if !strings.Contains(reserveScript, `EXPIRE`) {
		t.Fatal("reserve script should set ttl")
	}
}

func TestCommitScriptContainsReconciliation(t *testing.T) {
	if !strings.Contains(commitScript, `if actual > reserved`) {
		t.Fatal("commit script should handle extra charge path")
	}
	if !strings.Contains(commitScript, `elseif reserved > actual`) {
		t.Fatal("commit script should handle refund path")
	}
}
