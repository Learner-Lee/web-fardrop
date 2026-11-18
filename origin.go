package main

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	_ "modernc.org/sqlite"
)

type origin struct {
	db         *sql.DB
	root       string
	uploads    [64]sync.Mutex
	jobs       sync.Map
	assemblers chan struct{}
}

type assembly struct {
	done chan struct{}
	err  error
}

type upload struct {
	ID          string         `json:"id"`
	Size        int64          `json:"size"`
	ChunkSize   int            `json:"chunkSize"`
	Parts       map[int]string `json:"parts"`
	Name        string         `json:"name"`
	Expected    string         `json:"-"`
	Ready       bool           `json:"ready"`
	DeleteAfter int64          `json:"-"`
}

func openOrigin(root string) (*origin, error) {
	for _, dir := range []string{"parts", "objects"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0700); err != nil {
			return nil, err
		}
	}
	db, err := sql.Open("sqlite", filepath.Join(root, "metadata.sqlite"))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	_, err = db.Exec(`
		PRAGMA journal_mode = WAL;
		PRAGMA synchronous = FULL;
		PRAGMA foreign_keys = ON;
		CREATE TABLE IF NOT EXISTS uploads (
			id TEXT PRIMARY KEY, name TEXT NOT NULL, size INTEGER NOT NULL, fingerprint TEXT NOT NULL,
			expected TEXT NOT NULL, created INTEGER NOT NULL, sha256 TEXT NOT NULL DEFAULT '',
			UNIQUE (fingerprint)
		);
		CREATE TABLE IF NOT EXISTS parts (
			upload_id TEXT NOT NULL REFERENCES uploads(id), part INTEGER NOT NULL,
			sha256 TEXT NOT NULL, PRIMARY KEY (upload_id, part)
		);
		CREATE TABLE IF NOT EXISTS shares (
			id TEXT PRIMARY KEY, file_id TEXT NOT NULL REFERENCES uploads(id), token_hash TEXT NOT NULL UNIQUE,
			created INTEGER NOT NULL, expires INTEGER NOT NULL, revoked INTEGER NOT NULL DEFAULT 0
		);
	`)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := migrateOrigin(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &origin{db: db, root: root, assemblers: make(chan struct{}, 2)}, nil
}

func migrateOrigin(db *sql.DB) error {
	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version >= 2 {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if version == 0 {
		if _, err := tx.Exec("ALTER TABLE uploads ADD COLUMN chunk_size INTEGER NOT NULL DEFAULT 16777216"); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`ALTER TABLE uploads ADD COLUMN delete_after INTEGER;
		CREATE INDEX uploads_expiry ON uploads(delete_after) WHERE delete_after>0 AND sha256!='';
		CREATE TABLE file_deletions (id TEXT PRIMARY KEY, part_count INTEGER NOT NULL);
		PRAGMA user_version = 2;`); err != nil {
		return err
	}
	return tx.Commit()
}

func (o *origin) routes(secret, admin, peer string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /internal/health", func(w http.ResponseWriter, r *http.Request) {
		if err := o.db.PingContext(r.Context()); err != nil {
			fail(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /internal/authorize", o.authorize)
	mux.HandleFunc("GET /internal/objects/{id}", o.object)
	api := http.NewServeMux()
	api.HandleFunc("GET /api/files", o.files)
	api.HandleFunc("DELETE /api/files/{id}", o.deleteUploadedFile)
	api.HandleFunc("POST /api/uploads", o.start)
	api.HandleFunc("GET /api/uploads/{id}", o.progress)
	api.HandleFunc("PUT /api/uploads/{id}/parts/{part}", o.put)
	api.HandleFunc("POST /api/uploads/{id}/complete", o.complete)
	api.HandleFunc("POST /api/files/{id}/shares", o.share)
	api.HandleFunc("GET /api/files/{id}/shares", o.shares)
	api.HandleFunc("DELETE /api/shares/{id}", o.revoke)
	mux.Handle("/api/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !secretsMatch(r.Header.Get("Authorization"), "Bearer "+admin) {
			problem(w, http.StatusUnauthorized, "请输入有效的管理令牌。")
			return
		}
		api.ServeHTTP(w, r)
	}))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "private, no-store")
		host, _, _ := net.SplitHostPort(r.RemoteAddr)
		if host != peer || !secretsMatch(r.Header.Get(edgeHeader), secret) {
			http.NotFound(w, r)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func (o *origin) lookup(id string, parts bool) (upload, error) {
	result := upload{ID: id, Parts: map[int]string{}}
	if !validID(id, 16) {
		return result, sql.ErrNoRows
	}
	var digest string
	if err := o.db.QueryRow("SELECT name,size,expected,sha256,chunk_size,COALESCE(delete_after,0) FROM uploads WHERE id=?", id).Scan(&result.Name, &result.Size, &result.Expected, &digest, &result.ChunkSize, &result.DeleteAfter); err != nil {
		return result, err
	}
	result.Ready = digest != ""
	if !parts {
		return result, nil
	}
	rows, err := o.db.Query("SELECT part,sha256 FROM parts WHERE upload_id=?", id)
	if err != nil {
		return result, err
	}
	defer rows.Close()
	for rows.Next() {
		var index int
		var hash string
		if err := rows.Scan(&index, &hash); err != nil {
			return result, err
		}
		result.Parts[index] = hash
	}
	return result, rows.Err()
}

func (o *origin) lookupHTTP(w http.ResponseWriter, r *http.Request, parts bool) (upload, bool) {
	u, err := o.lookup(r.PathValue("id"), parts)
	if errors.Is(err, sql.ErrNoRows) {
		problem(w, http.StatusNotFound, "找不到上传任务。")
		return u, false
	}
	if err != nil {
		fail(w, err)
		return u, false
	}
	return u, true
}

func (o *origin) start(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name        string `json:"name"`
		Size        int64  `json:"size"`
		Fingerprint string `json:"fingerprint"`
		SHA256      string `json:"sha256"`
	}
	if !parse(w, r, &in) {
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" || len(in.Name) > 255 || strings.ContainsAny(in.Name, "\\/") || strings.IndexFunc(in.Name, unicode.IsControl) >= 0 ||
		in.Size <= 0 || in.Size > maxSize || !validID(in.Fingerprint, 32) || (in.SHA256 != "" && !validID(in.SHA256, 32)) {
		problem(w, http.StatusBadRequest, "文件名、大小或校验值无效。单文件上限为 5 GiB。")
		return
	}
	fingerprint := hashString(in.Name + "\x00" + strconv.FormatInt(in.Size, 10) + "\x00" + in.Fingerprint + "\x00" + in.SHA256)
	id := randomID(16)
	if _, err := o.db.Exec("INSERT INTO uploads(id,name,size,fingerprint,expected,created,chunk_size,delete_after) VALUES(?,?,?,?,?,?,?,0) ON CONFLICT(fingerprint) DO NOTHING",
		id, in.Name, in.Size, fingerprint, in.SHA256, time.Now().Unix(), uploadChunkSize); err != nil {
		fail(w, err)
		return
	}
	if err := o.db.QueryRow("SELECT id FROM uploads WHERE fingerprint=?", fingerprint).Scan(&id); err != nil {
		fail(w, err)
		return
	}
	u, err := o.lookup(id, true)
	if err != nil {
		fail(w, err)
		return
	}
	reply(w, http.StatusOK, u)
}

func (o *origin) progress(w http.ResponseWriter, r *http.Request) {
	if u, ok := o.lookupHTTP(w, r, true); ok {
		reply(w, http.StatusOK, u)
	}
}

func (o *origin) partPath(id string, part int) string {
	return filepath.Join(o.root, "parts", id+"-"+strconv.Itoa(part))
}

func (o *origin) lock(id string) func() {
	sum := sha256.Sum256([]byte(id))
	lock := &o.uploads[int(sum[0])%len(o.uploads)]
	lock.Lock()
	return lock.Unlock
}

func syncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func (o *origin) put(w http.ResponseWriter, r *http.Request) {
	index, err := strconv.Atoi(r.PathValue("part"))
	claimed := r.Header.Get("X-Chunk-SHA256")
	if err != nil || index < 0 || index >= maxSize/uploadChunkSize || !validID(claimed, 32) {
		problem(w, http.StatusBadRequest, "无效的分块编号或 SHA-256。")
		return
	}
	unlock := o.lock(r.PathValue("id"))
	defer unlock()
	u, ok := o.lookupHTTP(w, r, false)
	if !ok {
		return
	}
	size := partLength(u, index)
	if size <= 0 || (r.ContentLength >= 0 && r.ContentLength != size) {
		problem(w, http.StatusBadRequest, "分块大小不正确。")
		return
	}
	var previous string
	err = o.db.QueryRow("SELECT sha256 FROM parts WHERE upload_id=? AND part=?", u.ID, index).Scan(&previous)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		fail(w, err)
		return
	}
	if u.Ready || previous != "" {
		if previous == claimed {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		problem(w, http.StatusConflict, "这个分块已有其他内容。")
		return
	}
	dir := filepath.Join(o.root, "parts")
	out, err := os.CreateTemp(dir, ".incoming-*")
	if err != nil {
		fail(w, err)
		return
	}
	defer os.Remove(out.Name())
	defer out.Close()
	hash := sha256.New()
	if err := http.NewResponseController(w).SetReadDeadline(time.Now().Add(5 * time.Minute)); err == nil {
		defer http.NewResponseController(w).SetReadDeadline(time.Time{})
	}
	n, err := io.Copy(io.MultiWriter(out, hash), http.MaxBytesReader(w, r.Body, size))
	if err != nil || n != size || hex.EncodeToString(hash.Sum(nil)) != claimed {
		problem(w, http.StatusUnprocessableEntity, "分块未完整收到或校验失败。")
		return
	}
	if err := out.Sync(); err != nil {
		fail(w, err)
		return
	}
	if err := out.Close(); err != nil {
		fail(w, err)
		return
	}
	if err := os.Rename(out.Name(), o.partPath(u.ID, index)); err != nil {
		fail(w, err)
		return
	}
	if err := syncDir(dir); err != nil {
		fail(w, err)
		return
	}
	if _, err := o.db.Exec("INSERT INTO parts(upload_id,part,sha256) VALUES(?,?,?)", u.ID, index, claimed); err != nil {
		fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (o *origin) complete(w http.ResponseWriter, r *http.Request) {
	unlock := o.lock(r.PathValue("id"))
	defer unlock()
	u, ok := o.lookupHTTP(w, r, true)
	if !ok {
		return
	}
	count := int((u.Size + int64(u.ChunkSize) - 1) / int64(u.ChunkSize))
	if !u.Ready && len(u.Parts) != count {
		problem(w, http.StatusConflict, "还有分块尚未上传。")
		return
	}
	if !u.Ready {
		o.queueAssembly(w, u, count)
		return
	}
	var f File
	if err := o.db.QueryRow("SELECT id,name,size,sha256,created,COALESCE(delete_after,0) FROM uploads WHERE id=?", u.ID).Scan(&f.ID, &f.Name, &f.Size, &f.SHA256, &f.CreatedAt, &f.DeleteAfter); err != nil {
		fail(w, err)
		return
	}
	reply(w, http.StatusOK, f)
}

func (o *origin) queueAssembly(w http.ResponseWriter, u upload, count int) {
	if existing, ok := o.jobs.Load(u.ID); ok {
		job := existing.(*assembly)
		select {
		case <-job.done:
			if errors.Is(job.err, errChecksum) {
				problem(w, http.StatusUnprocessableEntity, "整文件 SHA-256 校验失败。")
				return
			}
			if job.err != nil {
				o.jobs.Delete(u.ID)
				fail(w, job.err)
				return
			}
		default:
		}
	} else {
		job := &assembly{done: make(chan struct{})}
		select {
		case o.assemblers <- struct{}{}:
		default:
			problem(w, http.StatusTooManyRequests, "服务器正在校验其他文件，请稍后重试。")
			return
		}
		o.jobs.Store(u.ID, job)
		go func() {
			defer func() { <-o.assemblers }()
			job.err = o.finish(u, count)
			close(job.done)
			if job.err == nil {
				unlock := o.lock(u.ID)
				o.jobs.Delete(u.ID)
				unlock()
			}
		}()
	}
	reply(w, http.StatusAccepted, map[string]bool{"pending": true})
}

func (o *origin) finish(u upload, count int) error {
	digest, err := o.assemble(u, count)
	if err != nil {
		return err
	}
	deleteAfter := time.Now().Add(fileRetention).Unix()
	if _, err := o.db.Exec(`UPDATE uploads SET sha256=?, delete_after=CASE WHEN delete_after=0 THEN ? ELSE delete_after END WHERE id=?`, digest, deleteAfter, u.ID); err != nil {
		return err
	}
	for index := 0; index < count; index++ {
		if err := os.Remove(o.partPath(u.ID, index)); err != nil && !errors.Is(err, os.ErrNotExist) {
			log.Printf("removing committed upload part: %v", err)
		}
	}
	return nil
}

var errChecksum = errors.New("checksum mismatch")

func (o *origin) assemble(u upload, count int) (string, error) {
	dir := filepath.Join(o.root, "objects")
	out, err := os.CreateTemp(dir, ".assembling-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(out.Name())
	defer out.Close()
	hash := sha256.New()
	for index := 0; index < count; index++ {
		in, err := os.Open(o.partPath(u.ID, index))
		if err != nil {
			return "", err
		}
		partHash := sha256.New()
		n, err := io.Copy(io.MultiWriter(out, hash, partHash), in)
		_ = in.Close()
		if err != nil {
			return "", err
		}
		if n != partLength(u, index) || hex.EncodeToString(partHash.Sum(nil)) != u.Parts[index] {
			return "", errChecksum
		}
	}
	digest := hex.EncodeToString(hash.Sum(nil))
	if u.Expected != "" && digest != u.Expected {
		return "", errChecksum
	}
	if err := out.Sync(); err != nil {
		return "", err
	}
	if err := out.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(out.Name(), filepath.Join(dir, u.ID)); err != nil {
		return "", err
	}
	return digest, syncDir(dir)
}

func (o *origin) files(w http.ResponseWriter, r *http.Request) {
	rows, err := o.db.Query("SELECT id,name,size,sha256,created,COALESCE(delete_after,0) FROM uploads WHERE sha256!='' ORDER BY created DESC,id DESC")
	if err != nil {
		fail(w, err)
		return
	}
	defer rows.Close()
	result := []File{}
	for rows.Next() {
		var f File
		if err := rows.Scan(&f.ID, &f.Name, &f.Size, &f.SHA256, &f.CreatedAt, &f.DeleteAfter); err != nil {
			fail(w, err)
			return
		}
		result = append(result, f)
	}
	if err := rows.Err(); err != nil {
		fail(w, err)
		return
	}
	reply(w, http.StatusOK, result)
}

func (o *origin) share(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Hours int `json:"hours"`
	}
	if !parse(w, r, &in) {
		return
	}
	if in.Hours < 1 || in.Hours > 720 {
		problem(w, http.StatusBadRequest, "有效期须为 1 到 720 小时。")
		return
	}
	unlock := o.lock(r.PathValue("id"))
	defer unlock()
	u, ok := o.lookupHTTP(w, r, false)
	if !ok {
		return
	}
	if !u.Ready {
		problem(w, http.StatusConflict, "文件还未完成上传。")
		return
	}
	now := time.Now().Unix()
	s := Share{ID: randomID(16), FileID: u.ID, ExpiresAt: now + int64(in.Hours)*3600, Token: randomID(32)}
	if u.DeleteAfter > 0 {
		if u.DeleteAfter <= now {
			problem(w, http.StatusGone, "文件已到期。")
			return
		}
		s.ExpiresAt = min(s.ExpiresAt, u.DeleteAfter)
	}
	if _, err := o.db.Exec("INSERT INTO shares(id,file_id,token_hash,created,expires) VALUES(?,?,?,?,?)",
		s.ID, s.FileID, hashString(s.Token), now, s.ExpiresAt); err != nil {
		fail(w, err)
		return
	}
	reply(w, http.StatusCreated, s)
}

func (o *origin) shares(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validID(id, 16) {
		problem(w, http.StatusNotFound, "找不到文件。")
		return
	}
	rows, err := o.db.Query("SELECT id,expires,revoked FROM shares WHERE file_id=? ORDER BY created DESC,id DESC", id)
	if err != nil {
		fail(w, err)
		return
	}
	defer rows.Close()
	result := []Share{}
	for rows.Next() {
		s := Share{FileID: id}
		if err := rows.Scan(&s.ID, &s.ExpiresAt, &s.RevokedAt); err != nil {
			fail(w, err)
			return
		}
		result = append(result, s)
	}
	if err := rows.Err(); err != nil {
		fail(w, err)
		return
	}
	sort.SliceStable(result, func(i, j int) bool { return result[i].RevokedAt == 0 && result[j].RevokedAt != 0 })
	reply(w, http.StatusOK, result)
}

func (o *origin) revoke(w http.ResponseWriter, r *http.Request) {
	if _, err := o.db.Exec("UPDATE shares SET revoked=? WHERE id=? AND revoked=0", time.Now().Unix(), r.PathValue("id")); err != nil {
		fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (o *origin) authorize(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Token string `json:"token"`
	}
	if !parse(w, r, &in) {
		return
	}
	if !validID(in.Token, 32) {
		problem(w, http.StatusNotFound, "链接无效或已失效。")
		return
	}
	var g Grant
	var revoked int64
	err := o.db.QueryRow(`
		SELECT u.id,u.name,u.size,u.sha256,u.created,MIN(s.expires,COALESCE(NULLIF(u.delete_after,0),s.expires)),s.revoked
		FROM shares s JOIN uploads u ON s.file_id=u.id WHERE s.token_hash=? AND u.sha256!=''
	`, hashString(in.Token)).Scan(&g.File.ID, &g.File.Name, &g.File.Size, &g.File.SHA256, &g.File.CreatedAt, &g.ExpiresAt, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		problem(w, http.StatusNotFound, "链接无效或已失效。")
		return
	}
	if err != nil {
		fail(w, err)
		return
	}
	if revoked != 0 || g.ExpiresAt <= time.Now().Unix() {
		problem(w, http.StatusGone, "链接已过期或已撤销。")
		return
	}
	reply(w, http.StatusOK, g)
}

func (o *origin) object(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validID(id, 16) {
		http.NotFound(w, r)
		return
	}
	data, err := os.Open(filepath.Join(o.root, "objects", id))
	if errors.Is(err, os.ErrNotExist) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		fail(w, err)
		return
	}
	defer data.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeContent(w, r, "", time.Time{}, data)
}
