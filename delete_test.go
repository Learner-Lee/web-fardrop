package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRetentionAppliesOnlyToNewUploads(t *testing.T) {
	root := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(root, "metadata.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	legacyID := randomID(16)
	legacyContent := "legacy"
	if _, err := db.Exec(`
		CREATE TABLE uploads (
			id TEXT PRIMARY KEY, name TEXT NOT NULL, size INTEGER NOT NULL,
			fingerprint TEXT NOT NULL UNIQUE, expected TEXT NOT NULL,
			created INTEGER NOT NULL, sha256 TEXT NOT NULL DEFAULT '',
			chunk_size INTEGER NOT NULL DEFAULT 16777216
		);
		PRAGMA user_version = 1;
		INSERT INTO uploads(id,name,size,fingerprint,expected,created,sha256) VALUES(?,?,?,?,?,?,?);
	`, legacyID, "legacy.bin", len(legacyContent), randomID(32), "", 1, hashString(legacyContent)); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	o := openTestOrigin(t, root)
	legacyPath := filepath.Join(root, "objects", legacyID)
	if err := os.WriteFile(legacyPath, []byte(legacyContent), 0600); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	file := newTestFile(t, o, "new.bin")
	if file.DeleteAfter < now.Add(fileRetention).Unix() || file.DeleteAfter > time.Now().Add(fileRetention).Unix() {
		t.Fatalf("deleteAfter = %d, expected completion time plus seven days", file.DeleteAfter)
	}
	share := newTestShare(t, o, file.ID)
	if share.ExpiresAt != file.DeleteAfter {
		t.Fatal("share lifetime exceeded file retention")
	}
	if _, err := o.db.Exec("UPDATE uploads SET delete_after=? WHERE id=?", now.Add(-time.Minute).Unix(), file.ID); err != nil {
		t.Fatal(err)
	}
	response := testOriginRequest(o, http.MethodPost, "/internal/authorize", `{"token":"`+share.Token+`"}`, false)
	if response.Code != http.StatusGone {
		t.Fatalf("expired file remained shareable before cleanup: %d", response.Code)
	}
	if err := o.removeDueFiles(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	assertPurged(t, o, file.ID)
	if got, err := os.ReadFile(legacyPath); err != nil || string(got) != legacyContent {
		t.Fatalf("legacy file affected by retention: %q, %v", got, err)
	}
	var deadline sql.NullInt64
	if err := o.db.QueryRow("SELECT delete_after FROM uploads WHERE id=?", legacyID).Scan(&deadline); err != nil || deadline.Valid {
		t.Fatalf("legacy file received a deadline: %v, %v", deadline, err)
	}
}

func TestManualDeletionAndRecovery(t *testing.T) {
	root := t.TempDir()
	o := openTestOrigin(t, root)
	file := newTestFile(t, o, "manual.bin")
	share := newTestShare(t, o, file.ID)
	path := "/api/files/" + file.ID
	if response := testOriginRequest(o, http.MethodDelete, path, "", false); response.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous delete returned %d", response.Code)
	}
	if response := testOriginRequest(o, http.MethodDelete, path, "", true); response.Code != http.StatusNoContent {
		t.Fatalf("admin delete returned %d: %s", response.Code, response.Body)
	}
	assertPurged(t, o, file.ID)
	if response := testOriginRequest(o, http.MethodPost, "/internal/authorize", `{"token":"`+share.Token+`"}`, false); response.Code != http.StatusNotFound {
		t.Fatalf("deleted share returned %d", response.Code)
	}
	if response := testOriginRequest(o, http.MethodDelete, path, "", true); response.Code != http.StatusNoContent {
		t.Fatalf("retry of deletion returned %d", response.Code)
	}
	retry := newTestFile(t, o, "manual.bin")
	if retry.ID == file.ID {
		t.Fatal("reupload reused a deleted object ID")
	}
	if pending, err := o.queueFileDeletion(retry.ID, 0); err != nil || pending != 1 {
		t.Fatalf("simulating interruption before unlink: count=%d err=%v", pending, err)
	}
	if err := o.db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openTestOrigin(t, root)
	if err := reopened.removeDueFiles(context.Background(), time.Now()); err != nil {
		t.Fatal(err)
	}
	assertPurged(t, reopened, retry.ID)
}

func openTestOrigin(t *testing.T, root string) *origin {
	t.Helper()
	o, err := openOrigin(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = o.db.Close() })
	return o
}

func newTestFile(t *testing.T, o *origin, name string) File {
	t.Helper()
	content := "example"
	body, err := json.Marshal(map[string]any{"name": name, "size": len(content), "fingerprint": hashString(name)})
	if err != nil {
		t.Fatal(err)
	}
	response := testOriginRequest(o, http.MethodPost, "/api/uploads", string(body), true)
	var u upload
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &u) != nil {
		t.Fatalf("start upload: %d %s", response.Code, response.Body)
	}
	partPath := o.partPath(u.ID, 0)
	if err := os.WriteFile(partPath, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	u.Parts[0] = hashString(content)
	if err := o.finish(u, 1); err != nil {
		t.Fatal(err)
	}
	response = testOriginRequest(o, http.MethodPost, "/api/uploads/"+u.ID+"/complete", "", true)
	var file File
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &file) != nil {
		t.Fatalf("complete upload: %d %s", response.Code, response.Body)
	}
	return file
}

func newTestShare(t *testing.T, o *origin, id string) Share {
	t.Helper()
	response := testOriginRequest(o, http.MethodPost, "/api/files/"+id+"/shares", `{"hours":720}`, true)
	var s Share
	if response.Code != http.StatusCreated || json.Unmarshal(response.Body.Bytes(), &s) != nil {
		t.Fatalf("create share: %d %s", response.Code, response.Body)
	}
	return s
}

func testOriginRequest(o *origin, method, path, body string, admin bool) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.RemoteAddr = "127.0.0.1:12345"
	r.Header.Set(edgeHeader, "edge-test")
	if admin {
		r.Header.Set("Authorization", "Bearer admin-test")
	}
	w := httptest.NewRecorder()
	o.routes("edge-test", "admin-test", "127.0.0.1").ServeHTTP(w, r)
	return w
}

func assertPurged(t *testing.T, o *origin, id string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(o.root, "objects", id)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("deleted object still exists: %v", err)
	}
	for _, table := range []string{"uploads", "file_deletions"} {
		var count int
		if err := o.db.QueryRow("SELECT count(*) FROM "+table+" WHERE id=?", id).Scan(&count); err != nil || count != 0 {
			t.Fatalf("row remained in %s: count=%d err=%v", table, count, err)
		}
	}
}
