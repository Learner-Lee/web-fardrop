package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

const fileRetention = 7 * 24 * time.Hour

var errUploadIncomplete = errors.New("upload is incomplete")

func (o *origin) deleteUploadedFile(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validID(id, 16) {
		problem(w, http.StatusNotFound, "找不到文件。")
		return
	}
	if err := o.deleteFile(id, 0); err != nil {
		if errors.Is(err, errUploadIncomplete) {
			problem(w, http.StatusConflict, "文件还未完成上传。")
			return
		}
		fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Record the deletion and revoke access before unlinking. A restart can then
// finish a partially completed deletion without restoring any share links.
func (o *origin) queueFileDeletion(id string, dueBy int64) (int, error) {
	tx, err := o.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	var count int
	var digest string
	var deleteAfter int64
	err = tx.QueryRow(`
		SELECT (size+chunk_size-1)/chunk_size, sha256, COALESCE(delete_after,0)
		FROM uploads WHERE id=?
	`, id).Scan(&count, &digest, &deleteAfter)
	if errors.Is(err, sql.ErrNoRows) {
		err := tx.QueryRow("SELECT part_count FROM file_deletions WHERE id=?", id).Scan(&count)
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return count, err
	}
	if err != nil {
		return 0, err
	}
	if dueBy != 0 && (deleteAfter <= 0 || deleteAfter > dueBy || digest == "") {
		return 0, nil
	}
	if digest == "" {
		return 0, errUploadIncomplete
	}
	if !validPartCount(count) {
		return 0, fmt.Errorf("invalid part count for %s", id)
	}
	if _, err := tx.Exec("INSERT INTO file_deletions(id,part_count) VALUES(?,?)", id, count); err != nil {
		return 0, err
	}
	if _, err := tx.Exec("DELETE FROM shares WHERE file_id=?", id); err != nil {
		return 0, err
	}
	if _, err := tx.Exec("DELETE FROM parts WHERE upload_id=?", id); err != nil {
		return 0, err
	}
	if _, err := tx.Exec("DELETE FROM uploads WHERE id=?", id); err != nil {
		return 0, err
	}
	return count, tx.Commit()
}

func validPartCount(count int) bool {
	return count > 0 && count <= maxSize/uploadChunkSize
}

func (o *origin) deleteFile(id string, dueBy int64) error {
	if !validID(id, 16) {
		return fmt.Errorf("invalid deletion ID %q", id)
	}
	unlock := o.lock(id)
	defer unlock()
	count, err := o.queueFileDeletion(id, dueBy)
	if err != nil || count == 0 {
		return err
	}
	if !validPartCount(count) {
		return fmt.Errorf("invalid queued part count for %s", id)
	}

	objects := filepath.Join(o.root, "objects")
	if err := unlinkFile(filepath.Join(objects, id)); err != nil {
		return err
	}
	for index := 0; index < count; index++ {
		if err := unlinkFile(o.partPath(id, index)); err != nil {
			return err
		}
	}
	if err := syncDir(objects); err != nil {
		return err
	}
	if err := syncDir(filepath.Join(o.root, "parts")); err != nil {
		return err
	}
	_, err = o.db.Exec("DELETE FROM file_deletions WHERE id=?", id)
	return err
}

func unlinkFile(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("refusing to unlink a non-regular file at %s", path)
	}
	if err := os.Remove(path); !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (o *origin) maintainFiles(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for ctx.Err() == nil {
		if err := o.removeDueFiles(ctx, time.Now()); err != nil && ctx.Err() == nil {
			log.Printf("checking FarDrop file deletions: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (o *origin) removeDueFiles(ctx context.Context, now time.Time) error {
	dueBy := now.Unix()
	rows, err := o.db.QueryContext(ctx, `
		SELECT id FROM file_deletions
		UNION
		SELECT id FROM uploads WHERE sha256!='' AND delete_after>0 AND delete_after<=?
	`, dueBy)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	_ = rows.Close()
	for _, id := range ids {
		if ctx.Err() != nil {
			break
		}
		if err := o.deleteFile(id, dueBy); err != nil {
			log.Printf("cleaning FarDrop file %s: %v", id, err)
		}
	}
	return nil
}
