package assets

import (
	"testing"
)

func TestEmbeddedReference(t *testing.T) {
	e, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	snapshot := e.Snapshot()
	if snapshot.BankSHA256 != BankHash || snapshot.ModelCount != 18 || !e.HasModel("gpt-6-astra") || e.HasModel("gpt-6.1-sol") {
		t.Fatal("wrong embedded bank")
	}
}
