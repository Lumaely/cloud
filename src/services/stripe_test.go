package services

import (
	"strings"
	"testing"
)

func TestMockStripeCreateCustomer(t *testing.T) {
	m := &MockStripeService{}

	id, err := m.CreateCustomer("a@b.c", "Alice")
	if err != nil {
		t.Fatalf("CreateCustomer: %v", err)
	}
	if !strings.HasPrefix(id, "cus_mock_") || len(id) != len("cus_mock_")+16 {
		t.Errorf("unexpected id %q", id)
	}

	again, _ := m.CreateCustomer("a@b.c", "Alice")
	if id != again {
		t.Error("id not deterministic")
	}
	other, _ := m.CreateCustomer("x@b.c", "Alice")
	if id == other {
		t.Error("different emails share an id")
	}
}
