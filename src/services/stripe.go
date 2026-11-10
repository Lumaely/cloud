package services

import (
	"crypto/sha256"
	"encoding/hex"
)

type PaymentService interface {
	CreateCustomer(email, fullName string) (string, error)
}

type MockStripeService struct{}

func (m *MockStripeService) CreateCustomer(email, fullName string) (string, error) {
	hash := sha256.Sum256([]byte(email + fullName))
	return "cus_mock_" + hex.EncodeToString(hash[:8]), nil
}
