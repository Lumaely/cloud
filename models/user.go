package models

import "time"

type User struct {
	UserID           string    `json:"user_id"`
	OAuthID          *string   `json:"oauth_id,omitempty"`
	Email            string    `json:"email"`
	StripeCustomerID string    `json:"stripe_customer_id"`
	CreatedAt        time.Time `json:"created_at"`
}

type RegisterUserRequest struct {
	Email    string  `json:"email" binding:"required"`
	Password *string `json:"password"`
	OAuthID  *string `json:"oauth_id"`
	FullName string  `json:"full_name"`
}

type LoginRequest struct {
	Email    string `json:"email" binding:"required"`
	Password string `json:"password" binding:"required"`
}

type AuthResponse struct {
	Token string `json:"token"`
	User  User   `json:"user"`
}
