package handlers

import (
	"cloud/models"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/jackc/pgx/v5/pgconn"
)

var userColumns = []string{"user_id", "oauth_id", "email", "stripe_customer_id", "created_at"}

func userRow() *sqlmock.Rows {
	return sqlmock.NewRows(userColumns).AddRow(testUserID, nil, "a@b.c", "cus_test", time.Now())
}

func expectSessionInsert(mock sqlmock.Sqlmock) {
	mock.ExpectExec("INSERT INTO user_sessions").
		WithArgs(testUserID, sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
}

func registerRequest(body string) *http.Request {
	return jsonRequest(http.MethodPost, "/users/register", body)
}

func TestRegisterUserWithPassword(t *testing.T) {
	env := newTestEnv(t)
	env.mock.ExpectQuery("INSERT INTO users").
		WithArgs(nil, "a@b.c", sqlmock.AnyArg(), "cus_test").
		WillReturnRows(userRow())
	expectSessionInsert(env.mock)

	w := serve(env.h.RegisterUser, registerRequest(`{"email":"a@b.c","password":"pw","full_name":"A"}`))

	assertStatus(t, w, http.StatusCreated)
	resp := decode[models.AuthResponse](t, w)
	if len(resp.Token) != 64 || resp.User.UserID != testUserID || resp.User.Email != "a@b.c" {
		t.Errorf("unexpected response %+v", resp)
	}
}

func TestRegisterUserWithOAuthOnly(t *testing.T) {
	env := newTestEnv(t)
	env.mock.ExpectQuery("INSERT INTO users").
		WithArgs("google-1", "a@b.c", nil, "cus_test").
		WillReturnRows(userRow())
	expectSessionInsert(env.mock)

	w := serve(env.h.RegisterUser, registerRequest(`{"email":"a@b.c","oauth_id":"google-1"}`))

	assertStatus(t, w, http.StatusCreated)
}

func TestRegisterUserErrors(t *testing.T) {
	cases := []struct {
		name   string
		body   string
		setup  func(*testEnv)
		status int
	}{
		{"invalid json", `{`, nil, http.StatusBadRequest},
		{"missing email", `{"password":"pw"}`, nil, http.StatusBadRequest},
		{"no credentials", `{"email":"a@b.c"}`, nil, http.StatusBadRequest},
		{"payment failure", `{"email":"a@b.c","password":"pw"}`, func(e *testEnv) {
			e.payment.err = errBoom
		}, http.StatusInternalServerError},
		{"duplicate user", `{"email":"a@b.c","password":"pw"}`, func(e *testEnv) {
			e.mock.ExpectQuery("INSERT INTO users").WillReturnError(&pgconn.PgError{Code: "23505"})
		}, http.StatusConflict},
		{"database error", `{"email":"a@b.c","password":"pw"}`, func(e *testEnv) {
			e.mock.ExpectQuery("INSERT INTO users").WillReturnError(errBoom)
		}, http.StatusInternalServerError},
		{"session error", `{"email":"a@b.c","password":"pw"}`, func(e *testEnv) {
			e.mock.ExpectQuery("INSERT INTO users").WillReturnRows(userRow())
			e.mock.ExpectExec("INSERT INTO user_sessions").WillReturnError(errBoom)
		}, http.StatusInternalServerError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newTestEnv(t)
			if tc.setup != nil {
				tc.setup(env)
			}
			assertStatus(t, serve(env.h.RegisterUser, registerRequest(tc.body)), tc.status)
		})
	}
}

func loginRow(passwordHash any) *sqlmock.Rows {
	return sqlmock.NewRows([]string{"user_id", "oauth_id", "email", "password_hash", "stripe_customer_id", "created_at"}).
		AddRow(testUserID, nil, "a@b.c", passwordHash, "cus_test", time.Now())
}

func loginRequest(body string) *http.Request {
	return jsonRequest(http.MethodPost, "/users/login", body)
}

func TestLoginSuccess(t *testing.T) {
	env := newTestEnv(t)
	hash, _ := env.h.Auth.HashPassword("pw")
	env.mock.ExpectQuery("FROM users").WithArgs("a@b.c").WillReturnRows(loginRow(hash))
	expectSessionInsert(env.mock)

	w := serve(env.h.Login, loginRequest(`{"email":"a@b.c","password":"pw"}`))

	assertStatus(t, w, http.StatusOK)
	resp := decode[models.AuthResponse](t, w)
	if len(resp.Token) != 64 || resp.User.UserID != testUserID {
		t.Errorf("unexpected response %+v", resp)
	}
}

func TestLoginErrors(t *testing.T) {
	validBody := `{"email":"a@b.c","password":"pw"}`
	cases := []struct {
		name   string
		body   string
		setup  func(*testEnv)
		status int
	}{
		{"invalid json", `{`, nil, http.StatusBadRequest},
		{"missing password", `{"email":"a@b.c"}`, nil, http.StatusBadRequest},
		{"unknown email", validBody, func(e *testEnv) {
			e.mock.ExpectQuery("FROM users").WillReturnError(sql.ErrNoRows)
		}, http.StatusUnauthorized},
		{"database error", validBody, func(e *testEnv) {
			e.mock.ExpectQuery("FROM users").WillReturnError(errBoom)
		}, http.StatusInternalServerError},
		{"oauth account without password", validBody, func(e *testEnv) {
			e.mock.ExpectQuery("FROM users").WillReturnRows(loginRow(nil))
		}, http.StatusUnauthorized},
		{"wrong password", validBody, func(e *testEnv) {
			hash, _ := e.h.Auth.HashPassword("other")
			e.mock.ExpectQuery("FROM users").WillReturnRows(loginRow(hash))
		}, http.StatusUnauthorized},
		{"session error", validBody, func(e *testEnv) {
			hash, _ := e.h.Auth.HashPassword("pw")
			e.mock.ExpectQuery("FROM users").WillReturnRows(loginRow(hash))
			e.mock.ExpectExec("INSERT INTO user_sessions").WillReturnError(errBoom)
		}, http.StatusInternalServerError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newTestEnv(t)
			if tc.setup != nil {
				tc.setup(env)
			}
			assertStatus(t, serve(env.h.Login, loginRequest(tc.body)), tc.status)
		})
	}
}

func meRequest(method string) *http.Request {
	return httptest.NewRequest(method, "/users/me", nil)
}

func TestGetUserProfile(t *testing.T) {
	env := newTestEnv(t)
	env.mock.ExpectQuery("FROM users").WithArgs(testUserID).WillReturnRows(userRow())

	w := serve(env.h.GetUserProfile, meRequest(http.MethodGet))

	assertStatus(t, w, http.StatusOK)
	if u := decode[models.User](t, w); u.UserID != testUserID || u.Email != "a@b.c" {
		t.Errorf("unexpected user %+v", u)
	}
}

func TestGetUserProfileErrors(t *testing.T) {
	cases := map[string]struct {
		err    error
		status int
	}{
		"not found":      {sql.ErrNoRows, http.StatusNotFound},
		"database error": {errBoom, http.StatusInternalServerError},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			env := newTestEnv(t)
			env.mock.ExpectQuery("FROM users").WillReturnError(tc.err)
			assertStatus(t, serve(env.h.GetUserProfile, meRequest(http.MethodGet)), tc.status)
		})
	}
}

func TestDeleteUserProfileRemovesObjectsFirst(t *testing.T) {
	env := newTestEnv(t)
	env.mock.ExpectExec("DELETE FROM users").WithArgs(testUserID).WillReturnResult(sqlmock.NewResult(0, 1))

	assertStatus(t, serve(env.h.DeleteUserProfile, meRequest(http.MethodDelete)), http.StatusNoContent)

	if got := env.storage.deletedPrefixes; len(got) != 1 || got[0] != testUserID+"/" {
		t.Errorf("deleted prefixes = %v, want [%s/]", got, testUserID)
	}
}

func TestDeleteUserProfileStorageErrorKeepsAccount(t *testing.T) {
	env := newTestEnv(t)
	env.storage.deletePrefixErr = errBoom

	assertStatus(t, serve(env.h.DeleteUserProfile, meRequest(http.MethodDelete)), http.StatusInternalServerError)
}

func TestDeleteUserProfile(t *testing.T) {
	cases := map[string]struct {
		setup  func(sqlmock.Sqlmock)
		status int
	}{
		"deleted": {func(m sqlmock.Sqlmock) {
			m.ExpectExec("DELETE FROM users").WithArgs(testUserID).WillReturnResult(sqlmock.NewResult(0, 1))
		}, http.StatusNoContent},
		"not found": {func(m sqlmock.Sqlmock) {
			m.ExpectExec("DELETE FROM users").WillReturnResult(sqlmock.NewResult(0, 0))
		}, http.StatusNotFound},
		"rows affected error": {func(m sqlmock.Sqlmock) {
			m.ExpectExec("DELETE FROM users").WillReturnResult(sqlmock.NewErrorResult(errBoom))
		}, http.StatusNotFound},
		"database error": {func(m sqlmock.Sqlmock) {
			m.ExpectExec("DELETE FROM users").WillReturnError(errBoom)
		}, http.StatusInternalServerError},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			env := newTestEnv(t)
			tc.setup(env.mock)
			assertStatus(t, serve(env.h.DeleteUserProfile, meRequest(http.MethodDelete)), tc.status)
		})
	}
}
