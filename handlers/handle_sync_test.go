package handlers

import (
	"cloud/models"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

const entryID = "22222222-2222-2222-2222-222222222222"

var entryColumns = []string{"entry_id", "mood_scale", "metadata_blob", "entry_timestamp", "sha256_hash", "updated_at", "deleted_at"}

var attachmentColumns = []string{"entry_id", "attachment_id", "encrypted_aes_key", "media_type"}

func expectNoAttachments(m sqlmock.Sqlmock) {
	m.ExpectQuery("FROM entry_attachments").WillReturnRows(sqlmock.NewRows(attachmentColumns))
}

func fetchRequest(query string) *http.Request {
	return httptest.NewRequest(http.MethodGet, "/sync"+query, nil)
}

func TestFetchDeltaReturnsEntries(t *testing.T) {
	env := newTestEnv(t)
	since := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	now := time.Now().UTC()
	env.mock.ExpectQuery("FROM mood_entries").
		WithArgs(testUserID, since).
		WillReturnRows(sqlmock.NewRows(entryColumns).
			AddRow(entryID, []byte("5"), []byte("meta"), now, "h1", now, nil).
			AddRow("deleted-id", []byte("3"), []byte("m"), now, "h2", now, now))
	env.mock.ExpectQuery("FROM entry_attachments").
		WithArgs(testUserID, since).
		WillReturnRows(sqlmock.NewRows(attachmentColumns).
			AddRow(entryID, "att-1", []byte("aes-1"), "image/png").
			AddRow(entryID, "att-2", []byte("aes-2"), "video/mp4"))

	w := serve(env.h.FetchDelta, fetchRequest("?since=2026-01-01T00:00:00Z"))

	assertStatus(t, w, http.StatusOK)
	entries := decode[[]models.MoodEntry](t, w)
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(entries))
	}
	if entries[0].EntryID != entryID || string(entries[0].MoodScale) != "5" || entries[0].DeletedAt != nil {
		t.Errorf("unexpected first entry %+v", entries[0])
	}
	if entries[1].DeletedAt == nil {
		t.Error("tombstone missing deleted_at")
	}
	att := entries[0].Attachments
	if len(att) != 2 || att[0].AttachmentID != "att-1" || string(att[0].EncryptedAESKey) != "aes-1" || att[1].MediaType != "video/mp4" {
		t.Errorf("unexpected attachments %+v", att)
	}
	if entries[1].Attachments == nil || len(entries[1].Attachments) != 0 {
		t.Errorf("entry without attachments = %#v, want empty slice", entries[1].Attachments)
	}
}

func TestFetchDeltaEmptyIsArray(t *testing.T) {
	env := newTestEnv(t)
	env.mock.ExpectQuery("FROM mood_entries").WillReturnRows(sqlmock.NewRows(entryColumns))
	expectNoAttachments(env.mock)

	w := serve(env.h.FetchDelta, fetchRequest("?since=2026-01-01T00:00:00Z"))

	assertStatus(t, w, http.StatusOK)
	if strings.TrimSpace(w.Body.String()) != "[]" {
		t.Errorf("body = %s, want []", w.Body.String())
	}
}

func TestFetchDeltaErrors(t *testing.T) {
	cases := []struct {
		name   string
		query  string
		setup  func(sqlmock.Sqlmock)
		status int
	}{
		{"missing since", "", nil, http.StatusBadRequest},
		{"invalid since", "?since=yesterday", nil, http.StatusBadRequest},
		{"query error", "?since=2026-01-01T00:00:00Z", func(m sqlmock.Sqlmock) {
			m.ExpectQuery("FROM mood_entries").WillReturnError(errBoom)
		}, http.StatusInternalServerError},
		{"scan error", "?since=2026-01-01T00:00:00Z", func(m sqlmock.Sqlmock) {
			m.ExpectQuery("FROM mood_entries").WillReturnRows(sqlmock.NewRows(entryColumns).
				AddRow(entryID, []byte("5"), []byte("m"), "not-a-time", "h", time.Now(), nil))
		}, http.StatusInternalServerError},
		{"iteration error", "?since=2026-01-01T00:00:00Z", func(m sqlmock.Sqlmock) {
			m.ExpectQuery("FROM mood_entries").WillReturnRows(sqlmock.NewRows(entryColumns).
				AddRow(entryID, []byte("5"), []byte("m"), time.Now(), "h", time.Now(), nil).
				RowError(0, errBoom))
		}, http.StatusInternalServerError},
		{"attachments query error", "?since=2026-01-01T00:00:00Z", func(m sqlmock.Sqlmock) {
			m.ExpectQuery("SELECT entry_id, mood_scale").WillReturnRows(sqlmock.NewRows(entryColumns))
			m.ExpectQuery("FROM entry_attachments").WillReturnError(errBoom)
		}, http.StatusInternalServerError},
		{"attachments scan error", "?since=2026-01-01T00:00:00Z", func(m sqlmock.Sqlmock) {
			m.ExpectQuery("SELECT entry_id, mood_scale").WillReturnRows(sqlmock.NewRows(entryColumns))
			m.ExpectQuery("FROM entry_attachments").WillReturnRows(sqlmock.NewRows([]string{"entry_id"}).AddRow("x"))
		}, http.StatusInternalServerError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newTestEnv(t)
			if tc.setup != nil {
				tc.setup(env.mock)
			}
			assertStatus(t, serve(env.h.FetchDelta, fetchRequest(tc.query)), tc.status)
		})
	}
}

func entryFields(id string) map[string][]string {
	f := map[string][]string{
		"mood_scale":      {"5"},
		"metadata_blob":   {"meta"},
		"sha256_hash":     {"hash"},
		"entry_timestamp": {"2026-11-09T10:00:00Z"},
	}
	if id != "" {
		f["entry_id"] = []string{id}
	}
	return f
}

func expectNoDuplicate(m sqlmock.Sqlmock) {
	m.ExpectQuery("SELECT entry_id FROM mood_entries").
		WithArgs(testUserID, "hash").
		WillReturnError(sql.ErrNoRows)
}

func expectInsertEntry(m sqlmock.Sqlmock) {
	m.ExpectQuery("INSERT INTO mood_entries").
		WithArgs(testUserID, []byte("5"), []byte("meta"), time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC), "hash").
		WillReturnRows(sqlmock.NewRows([]string{"entry_id"}).AddRow(entryID))
}

func TestPushChangesCreatesEntry(t *testing.T) {
	env := newTestEnv(t)
	expectNoDuplicate(env.mock)
	env.mock.ExpectBegin()
	expectInsertEntry(env.mock)
	env.mock.ExpectCommit()

	w := serve(env.h.PushChanges, multipartRequest(t, entryFields(""), nil))

	assertStatus(t, w, http.StatusCreated)
	if got := decode[map[string]string](t, w); got["entry_id"] != entryID || got["status"] != "synced" {
		t.Errorf("unexpected body %v", got)
	}
}

func TestPushChangesUpdatesEntry(t *testing.T) {
	env := newTestEnv(t)
	env.mock.ExpectQuery("SELECT entry_id FROM mood_entries").
		WillReturnRows(sqlmock.NewRows([]string{"entry_id"}).AddRow(entryID))
	env.mock.ExpectBegin()
	env.mock.ExpectExec("UPDATE mood_entries").
		WithArgs([]byte("5"), []byte("meta"), sqlmock.AnyArg(), "hash", entryID, testUserID).
		WillReturnResult(sqlmock.NewResult(0, 1))
	env.mock.ExpectCommit()

	w := serve(env.h.PushChanges, multipartRequest(t, entryFields(entryID), nil))

	assertStatus(t, w, http.StatusCreated)
	if got := decode[map[string]string](t, w)["entry_id"]; got != entryID {
		t.Errorf("entry_id = %q, want %q", got, entryID)
	}
}

func TestPushChangesUploadsAttachments(t *testing.T) {
	env := newTestEnv(t)
	expectNoDuplicate(env.mock)
	env.mock.ExpectBegin()
	expectInsertEntry(env.mock)
	env.mock.ExpectExec("INSERT INTO entry_attachments").
		WithArgs(entryID, sqlmock.AnyArg(), []byte("key-1"), "image/png").
		WillReturnResult(sqlmock.NewResult(0, 1))
	env.mock.ExpectExec("INSERT INTO entry_attachments").
		WithArgs(entryID, sqlmock.AnyArg(), []byte("key-2"), "application/octet-stream").
		WillReturnResult(sqlmock.NewResult(0, 1))
	env.mock.ExpectCommit()

	fields := entryFields("")
	fields["encrypted_aes_key"] = []string{"key-1", "key-2"}
	fields["media_type"] = []string{"image/png"}
	req := multipartRequest(t, fields, []fileField{{"a.png", "first"}, {"b.bin", "second"}})

	w := serve(env.h.PushChanges, req)

	assertStatus(t, w, http.StatusCreated)
	up := env.storage.uploads
	if len(up) != 2 {
		t.Fatalf("got %d uploads, want 2", len(up))
	}
	keyPattern := regexp.MustCompile("^" + testUserID + "/" + entryID + "/[0-9a-f]{32}$")
	for i, want := range []upload{{"", "image/png", "first"}, {"", "application/octet-stream", "second"}} {
		if !keyPattern.MatchString(up[i].key) || up[i].contentType != want.contentType || up[i].body != want.body {
			t.Errorf("upload %d = %+v", i, up[i])
		}
	}
	if len(env.storage.deleted) != 0 {
		t.Errorf("committed objects deleted: %v", env.storage.deleted)
	}
}

func deleteRequest(t *testing.T) *http.Request {
	return multipartRequest(t, map[string][]string{"entry_id": {entryID}, "is_deleted": {"true"}}, nil)
}

func TestPushChangesDeletesEntry(t *testing.T) {
	env := newTestEnv(t)
	env.mock.ExpectBegin()
	env.mock.ExpectExec("SET deleted_at = NOW()").
		WithArgs(entryID, testUserID).
		WillReturnResult(sqlmock.NewResult(0, 1))
	env.mock.ExpectExec("DELETE FROM entry_attachments").
		WithArgs(entryID).
		WillReturnResult(sqlmock.NewResult(0, 2))
	env.mock.ExpectCommit()

	w := serve(env.h.PushChanges, multipartRequest(t, map[string][]string{
		"entry_id": {entryID}, "is_deleted": {"true"},
	}, nil))

	assertStatus(t, w, http.StatusOK)
	if got := decode[map[string]string](t, w)["status"]; got != "deleted" {
		t.Errorf("status = %q, want deleted", got)
	}
	if got := env.storage.deletedPrefixes; len(got) != 1 || got[0] != testUserID+"/"+entryID+"/" {
		t.Errorf("deleted prefixes = %v", got)
	}
}

func TestPushChangesDeleteErrors(t *testing.T) {
	cases := []struct {
		name   string
		setup  func(*testEnv)
		status int
	}{
		{"begin error", func(e *testEnv) {
			e.mock.ExpectBegin().WillReturnError(errBoom)
		}, http.StatusInternalServerError},
		{"tombstone error", func(e *testEnv) {
			e.mock.ExpectBegin()
			e.mock.ExpectExec("SET deleted_at = NOW()").WillReturnError(errBoom)
			e.mock.ExpectRollback()
		}, http.StatusInternalServerError},
		{"unknown entry", func(e *testEnv) {
			e.mock.ExpectBegin()
			e.mock.ExpectExec("SET deleted_at = NOW()").WillReturnResult(sqlmock.NewResult(0, 0))
			e.mock.ExpectRollback()
		}, http.StatusNotFound},
		{"attachment rows error", func(e *testEnv) {
			e.mock.ExpectBegin()
			e.mock.ExpectExec("SET deleted_at = NOW()").WillReturnResult(sqlmock.NewResult(0, 1))
			e.mock.ExpectExec("DELETE FROM entry_attachments").WillReturnError(errBoom)
			e.mock.ExpectRollback()
		}, http.StatusInternalServerError},
		{"storage error rolls back", func(e *testEnv) {
			e.storage.deletePrefixErr = errBoom
			e.mock.ExpectBegin()
			e.mock.ExpectExec("SET deleted_at = NOW()").WillReturnResult(sqlmock.NewResult(0, 1))
			e.mock.ExpectExec("DELETE FROM entry_attachments").WillReturnResult(sqlmock.NewResult(0, 1))
			e.mock.ExpectRollback()
		}, http.StatusInternalServerError},
		{"commit error", func(e *testEnv) {
			e.mock.ExpectBegin()
			e.mock.ExpectExec("SET deleted_at = NOW()").WillReturnResult(sqlmock.NewResult(0, 1))
			e.mock.ExpectExec("DELETE FROM entry_attachments").WillReturnResult(sqlmock.NewResult(0, 1))
			e.mock.ExpectCommit().WillReturnError(errBoom)
		}, http.StatusInternalServerError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newTestEnv(t)
			tc.setup(env)
			assertStatus(t, serve(env.h.PushChanges, deleteRequest(t)), tc.status)
		})
	}
}

func TestPushChangesRequestErrors(t *testing.T) {
	cases := []struct {
		name   string
		req    func(*testing.T) *http.Request
		setup  func(sqlmock.Sqlmock)
		status int
	}{
		{"not multipart", func(*testing.T) *http.Request {
			return jsonRequest(http.MethodPost, "/sync", `{}`)
		}, nil, http.StatusBadRequest},
		{"delete without entry_id", func(t *testing.T) *http.Request {
			return multipartRequest(t, map[string][]string{"is_deleted": {"true"}}, nil)
		}, nil, http.StatusBadRequest},
		{"malformed entry_id", func(t *testing.T) *http.Request {
			return multipartRequest(t, entryFields("not-a-uuid"), nil)
		}, nil, http.StatusBadRequest},
		{"update unknown entry", func(t *testing.T) *http.Request {
			return multipartRequest(t, entryFields(entryID), nil)
		}, func(m sqlmock.Sqlmock) {
			expectNoDuplicate(m)
			m.ExpectBegin()
			m.ExpectExec("UPDATE mood_entries").WillReturnResult(sqlmock.NewResult(0, 0))
			m.ExpectRollback()
		}, http.StatusNotFound},
		{"invalid timestamp", func(t *testing.T) *http.Request {
			f := entryFields("")
			f["entry_timestamp"] = []string{"today"}
			return multipartRequest(t, f, nil)
		}, nil, http.StatusBadRequest},
		{"file without aes key", func(t *testing.T) *http.Request {
			return multipartRequest(t, entryFields(""), []fileField{{"a", "x"}})
		}, nil, http.StatusBadRequest},
		{"duplicate check error", func(t *testing.T) *http.Request {
			return multipartRequest(t, entryFields(""), nil)
		}, func(m sqlmock.Sqlmock) {
			m.ExpectQuery("SELECT entry_id FROM mood_entries").WillReturnError(errBoom)
		}, http.StatusInternalServerError},
		{"duplicate hash", func(t *testing.T) *http.Request {
			return multipartRequest(t, entryFields(""), nil)
		}, func(m sqlmock.Sqlmock) {
			m.ExpectQuery("SELECT entry_id FROM mood_entries").
				WillReturnRows(sqlmock.NewRows([]string{"entry_id"}).AddRow("other-entry"))
		}, http.StatusConflict},
		{"begin error", func(t *testing.T) *http.Request {
			return multipartRequest(t, entryFields(""), nil)
		}, func(m sqlmock.Sqlmock) {
			expectNoDuplicate(m)
			m.ExpectBegin().WillReturnError(errBoom)
		}, http.StatusInternalServerError},
		{"insert error", func(t *testing.T) *http.Request {
			return multipartRequest(t, entryFields(""), nil)
		}, func(m sqlmock.Sqlmock) {
			expectNoDuplicate(m)
			m.ExpectBegin()
			m.ExpectQuery("INSERT INTO mood_entries").WillReturnError(errBoom)
			m.ExpectRollback()
		}, http.StatusInternalServerError},
		{"update error", func(t *testing.T) *http.Request {
			return multipartRequest(t, entryFields(entryID), nil)
		}, func(m sqlmock.Sqlmock) {
			expectNoDuplicate(m)
			m.ExpectBegin()
			m.ExpectExec("UPDATE mood_entries").WillReturnError(errBoom)
			m.ExpectRollback()
		}, http.StatusInternalServerError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newTestEnv(t)
			if tc.setup != nil {
				tc.setup(env.mock)
			}
			assertStatus(t, serve(env.h.PushChanges, tc.req(t)), tc.status)
		})
	}
}

func TestPushChangesCleansUpObjectsOnFailure(t *testing.T) {
	twoFiles := []fileField{{"a", "first"}, {"b", "second"}}
	cases := []struct {
		name        string
		failOnNth   int
		setup       func(sqlmock.Sqlmock)
		wantDeleted int
	}{
		{"second upload fails", 2, func(m sqlmock.Sqlmock) {
			m.ExpectExec("INSERT INTO entry_attachments").WillReturnResult(sqlmock.NewResult(0, 1))
			m.ExpectRollback()
		}, 1},
		{"attachment insert fails", 0, func(m sqlmock.Sqlmock) {
			m.ExpectExec("INSERT INTO entry_attachments").WillReturnError(errBoom)
			m.ExpectRollback()
		}, 1},
		{"commit fails", 0, func(m sqlmock.Sqlmock) {
			m.ExpectExec("INSERT INTO entry_attachments").WillReturnResult(sqlmock.NewResult(0, 1))
			m.ExpectExec("INSERT INTO entry_attachments").WillReturnResult(sqlmock.NewResult(0, 1))
			m.ExpectCommit().WillReturnError(errBoom)
		}, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newTestEnv(t)
			env.storage.failOnNth = tc.failOnNth
			expectNoDuplicate(env.mock)
			env.mock.ExpectBegin()
			expectInsertEntry(env.mock)
			tc.setup(env.mock)

			fields := entryFields("")
			fields["encrypted_aes_key"] = []string{"k1", "k2"}
			w := serve(env.h.PushChanges, multipartRequest(t, fields, twoFiles))

			assertStatus(t, w, http.StatusInternalServerError)
			if len(env.storage.deleted) != tc.wantDeleted {
				t.Fatalf("deleted %v, want %d objects", env.storage.deleted, tc.wantDeleted)
			}
			for i, key := range env.storage.deleted {
				if key != env.storage.uploads[i].key {
					t.Errorf("deleted %q, want %q", key, env.storage.uploads[i].key)
				}
			}
		})
	}
}

func TestDeleteObjectsContinuesOnError(t *testing.T) {
	env := newTestEnv(t)
	env.storage.deleteErr = errBoom

	env.h.deleteObjects([]string{"a", "b"})

	if len(env.storage.deleted) != 2 {
		t.Errorf("deleted %v, want both keys attempted", env.storage.deleted)
	}
}

func TestNewObjectKey(t *testing.T) {
	a := newObjectKey("user", "entry")
	b := newObjectKey("user", "entry")

	if !regexp.MustCompile(`^user/entry/[0-9a-f]{32}$`).MatchString(a) {
		t.Errorf("key %q has wrong format", a)
	}
	if a == b {
		t.Error("keys are not unique")
	}
}
