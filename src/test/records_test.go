package test

import (
	"bytes"
	"cloud/src/models"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
)

var collections = []string{"entries", "patterns", "activities", "places", "bookmark-folders", "bookmarks"}

func dayID(t *testing.T) string {
	return hex.EncodeToString(randomBytes(t, 32))
}

func uuidV7(t *testing.T) string {
	id, err := uuid.NewV7()
	if err != nil {
		t.Fatal(err)
	}
	return id.String()
}

func (s *stack) put(token, collection, id string, version int, blob string) *httptest.ResponseRecorder {
	s.t.Helper()
	return s.do(http.MethodPut, "/"+collection+"/"+id, token, map[string]any{"version": version, "blob": []byte(blob)})
}

func (s *stack) del(token, collection, id string, version int) *httptest.ResponseRecorder {
	s.t.Helper()
	return s.do(http.MethodDelete, "/"+collection+"/"+id+"?version="+strconv.Itoa(version), token, nil)
}

func (s *stack) create(token, collection, id, blob string) models.Record {
	s.t.Helper()
	w := s.put(token, collection, id, 0, blob)
	expect(s.t, w, http.StatusCreated)
	return decode[models.Record](s.t, w)
}

func (s *stack) changes(token, query string) models.Page[models.Record] {
	s.t.Helper()
	w := s.do(http.MethodGet, "/changes"+query, token, nil)
	expect(s.t, w, http.StatusOK)
	return decode[models.Page[models.Record]](s.t, w)
}

func (s *stack) pushChanges(token string, changes []map[string]any) []models.ChangeResult {
	s.t.Helper()
	w := s.do(http.MethodPost, "/changes", token, map[string]any{"changes": changes})
	expect(s.t, w, http.StatusOK)
	return decode[struct {
		Results []models.ChangeResult `json:"results"`
	}](s.t, w).Results
}

type conflictBody struct {
	Current models.Record `json:"current"`
}

func TestIntegrationRecordLifecycle(t *testing.T) {
	s := newStack(t, 0)
	a := s.register("records@test.local")

	for _, col := range collections {
		t.Run(col, func(t *testing.T) {
			id := uuidV7(t)
			rec := s.create(a.token, col, id, "v1")
			if rec.Collection != col || rec.ID != id || rec.Version != 1 || string(rec.Blob) != "v1" || rec.DeletedAt != nil {
				t.Fatalf("created = %+v", rec)
			}

			w := s.put(a.token, col, id, 0, "again")
			expect(t, w, http.StatusConflict)
			if cur := decode[conflictBody](t, w).Current; cur.Version != 1 || string(cur.Blob) != "v1" {
				t.Errorf("second create conflict = %+v", cur)
			}

			w = s.put(a.token, col, id, 1, "v2")
			expect(t, w, http.StatusOK)
			if got := decode[models.Record](t, w); got.Version != 2 || got.Seq <= rec.Seq {
				t.Errorf("updated = %+v", got)
			}
			expect(t, s.put(a.token, col, id, 1, "stale"), http.StatusConflict)

			expect(t, s.do(http.MethodDelete, "/"+col+"/"+id, a.token, nil), http.StatusBadRequest)
			w = s.del(a.token, col, id, 1)
			expect(t, w, http.StatusConflict)
			if cur := decode[conflictBody](t, w).Current; string(cur.Blob) != "v2" {
				t.Errorf("stale delete conflict = %+v", cur)
			}
			expect(t, s.del(a.token, col, id, 2), http.StatusNoContent)
			expect(t, s.del(a.token, col, id, 3), http.StatusNotFound)
			expect(t, s.del(a.token, col, uuidV7(t), 1), http.StatusNotFound)
			expect(t, s.put(a.token, col, uuidV7(t), 4, "unknown"), http.StatusNotFound)

			w = s.put(a.token, col, id, 0, "recreate")
			expect(t, w, http.StatusConflict)
			tomb := decode[conflictBody](t, w).Current
			if tomb.Version != 3 || tomb.DeletedAt == nil || len(tomb.Blob) != 0 {
				t.Errorf("tombstone = %+v", tomb)
			}
			w = s.put(a.token, col, id, 3, "revived")
			expect(t, w, http.StatusOK)
			if got := decode[models.Record](t, w); got.Version != 4 || got.DeletedAt != nil || string(got.Blob) != "revived" {
				t.Errorf("revived = %+v", got)
			}
		})
	}
}

func TestIntegrationRecordValidation(t *testing.T) {
	s := newStack(t, 0)
	a := s.register("ids@test.local")

	s.create(a.token, "entries", dayID(t), "day")
	s.create(a.token, "patterns", dayID(t), "hex ids are valid in every collection")
	s.create(a.token, "entries", uuidV7(t), "uuid ids too")

	upperUUID := strings.ToUpper(uuidV7(t))
	for _, id := range []string{
		"2026-10-29",
		strings.ToUpper(dayID(t)),
		dayID(t)[:63],
		dayID(t) + "0",
		"urn:uuid:" + uuidV7(t),
		upperUUID,
		strings.ReplaceAll(uuidV7(t), "-", ""),
	} {
		expect(t, s.put(a.token, "entries", id, 0, "x"), http.StatusBadRequest)
	}
	expect(t, s.put(a.token, "entries", dayID(t), 0, ""), http.StatusBadRequest)
	expect(t, s.put(a.token, "entries", dayID(t), -1, "x"), http.StatusBadRequest)
	expect(t, s.put(a.token, "entries", dayID(t), 0, strings.Repeat("x", 256<<10+1)), http.StatusBadRequest)
	expect(t, s.put(a.token, "entries", dayID(t), 0, strings.Repeat("x", 256<<10)), http.StatusCreated)
	expect(t, s.do(http.MethodDelete, "/entries/"+dayID(t)+"?version=0", a.token, nil), http.StatusBadRequest)
	expect(t, s.do(http.MethodDelete, "/entries/"+dayID(t)+"?version=x", a.token, nil), http.StatusBadRequest)
	expect(t, s.put(a.token, "moods", dayID(t), 0, "x"), http.StatusNotFound)
}

func TestIntegrationRecordsAreIsolated(t *testing.T) {
	s := newStack(t, 0)
	a, b := s.register("iso-a@test.local"), s.register("iso-b@test.local")
	id := dayID(t)
	s.create(a.token, "entries", id, "a's day")

	if got := s.create(b.token, "entries", id, "b's day"); string(got.Blob) != "b's day" {
		t.Errorf("b's copy = %+v", got)
	}
	shared := uuidV7(t)
	s.create(a.token, "patterns", shared, "a")
	expect(t, s.put(b.token, "patterns", shared, 1, "steal"), http.StatusNotFound)
	expect(t, s.del(b.token, "patterns", shared, 1), http.StatusNotFound)

	for _, rec := range s.changes(b.token, "").Items {
		if string(rec.Blob) == "a's day" || string(rec.Blob) == "a" {
			t.Errorf("b sees a's record %+v", rec)
		}
	}
	if got := s.changes(a.token, ""); len(got.Items) != 2 || string(got.Items[0].Blob) != "a's day" {
		t.Errorf("a's feed = %+v", got.Items)
	}
}

func TestIntegrationChangeFeed(t *testing.T) {
	s := newStack(t, 0)
	a := s.register("feed@test.local")
	day, pattern, folder := dayID(t), uuidV7(t), uuidV7(t)
	s.create(a.token, "entries", day, "e")
	s.create(a.token, "patterns", pattern, "p")
	s.create(a.token, "bookmark-folders", folder, "f")
	expect(t, s.put(a.token, "entries", day, 1, "e2"), http.StatusOK)

	first := s.changes(a.token, "?limit=2")
	if len(first.Items) != 2 || !first.HasMore || first.Total != 3 || first.DiaryEpoch != 1 ||
		first.Items[0].Collection != "patterns" || first.Items[1].Collection != "bookmark-folders" {
		t.Fatalf("first page = %+v", first)
	}
	rest := s.changes(a.token, "?cursor="+strconv.FormatInt(first.NextCursor, 10))
	if len(rest.Items) != 1 || rest.HasMore || rest.Items[0].ID != day || rest.Items[0].Version != 2 || rest.Total != 1 {
		t.Fatalf("second page = %+v", rest)
	}
	if empty := s.changes(a.token, "?cursor="+strconv.FormatInt(rest.NextCursor, 10)); len(empty.Items) != 0 || empty.NextCursor != rest.NextCursor {
		t.Errorf("caught-up page = %+v", empty)
	}
	expect(t, s.do(http.MethodGet, "/changes?limit=501", a.token, nil), http.StatusBadRequest)

	newDay := dayID(t)
	results := s.pushChanges(a.token, []map[string]any{
		{"collection": "entries", "id": newDay, "version": 0, "blob": []byte("new")},
		{"collection": "entries", "id": day, "version": 2, "blob": []byte("e3")},
		{"collection": "patterns", "id": pattern, "version": 9, "blob": []byte("stale")},
		{"collection": "bookmark-folders", "id": folder, "version": 1, "deleted": true},
		{"collection": "places", "id": uuidV7(t), "version": 1, "deleted": true},
		{"collection": "moods", "id": uuidV7(t), "version": 0, "blob": []byte("x")},
		{"collection": "entries", "id": "BAD", "version": 0, "blob": []byte("x")},
	})
	want := []string{"created", "updated", "conflict", "deleted", "not_found", "invalid", "invalid"}
	if len(results) != len(want) {
		t.Fatalf("results = %+v", results)
	}
	for i, r := range results {
		if r.Status != want[i] {
			t.Errorf("result %d = %+v, want %s", i, r, want[i])
		}
	}
	if results[0].Item == nil || results[0].Item.Version != 1 || results[0].Collection != "entries" || results[0].ID != newDay {
		t.Errorf("created result = %+v", results[0])
	}
	if results[2].Item == nil || string(results[2].Item.Blob) != "p" {
		t.Errorf("conflict result = %+v", results[2])
	}
	if results[3].Item != nil {
		t.Errorf("delete result carries an item: %+v", results[3])
	}

	tail := s.changes(a.token, "?cursor="+strconv.FormatInt(rest.NextCursor, 10))
	if len(tail.Items) != 3 || tail.Items[2].DeletedAt == nil || tail.Items[2].Collection != "bookmark-folders" {
		t.Errorf("feed after push = %+v", tail.Items)
	}

	expect(t, s.do(http.MethodPost, "/changes", a.token, map[string]any{"changes": []any{}}), http.StatusBadRequest)
	tooMany := make([]map[string]any, 101)
	for i := range tooMany {
		tooMany[i] = map[string]any{"collection": "patterns", "id": uuidV7(t), "version": 0, "blob": []byte("x")}
	}
	expect(t, s.do(http.MethodPost, "/changes", a.token, map[string]any{"changes": tooMany}), http.StatusBadRequest)
	req := httptest.NewRequest(http.MethodPost, "/changes", bytes.NewReader([]byte("not json")))
	req.Header.Set("Authorization", "Bearer "+a.token)
	w := httptest.NewRecorder()
	s.router.ServeHTTP(w, req)
	expect(t, w, http.StatusBadRequest)
}

func TestIntegrationConcurrentCreatesOfOneDay(t *testing.T) {
	s := newStack(t, 0)
	phone := s.register("race@test.local")
	tablet := s.loginOn(phone, "Tablet", "android")
	id := dayID(t)

	codes := make(chan int, 2)
	var wg sync.WaitGroup
	for _, token := range []string{phone.token, tablet} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes <- s.put(token, "entries", id, 0, "offline day").Code
		}()
	}
	wg.Wait()
	close(codes)

	got := map[int]int{}
	for c := range codes {
		got[c]++
	}
	if got[http.StatusCreated] != 1 || got[http.StatusConflict] != 1 {
		t.Errorf("statuses = %v, want one 201 and one 409", got)
	}
}
