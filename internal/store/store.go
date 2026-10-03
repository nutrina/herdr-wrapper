// Package store persists tasks, their labels, attached files and agent runs.
// Metadata lives in SQLite; attached files live on disk under <dir>/files/<task id>/.
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

var ErrNotFound = errors.New("not found")

const (
	StatusDraft   = "draft"
	StatusQueued  = "queued"
	StatusRunning = "running"
	StatusInput   = "input"
	StatusDone    = "done"
	StatusFailed  = "failed"
)

type Task struct {
	ID          int64
	Title       string
	Description string
	Category    string
	Status      string
	Memory      string
	Labels      []string
	CreatedAt   time.Time
	UpdatedAt   time.Time

	// Only filled in by ListTasks.
	FileCount int
	RunCount  int
}

type Attachment struct {
	ID        int64
	TaskID    int64
	Name      string
	Size      int64
	CreatedAt time.Time
}

type Run struct {
	ID        int64
	TaskID    int64
	Number    int
	Status    string
	Summary   string
	StartedAt time.Time
	EndedAt   *time.Time
}

type Store struct {
	db  *sql.DB
	dir string
}

const schema = `
CREATE TABLE IF NOT EXISTS tasks (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	title       TEXT NOT NULL,
	description TEXT NOT NULL DEFAULT '',
	category    TEXT NOT NULL DEFAULT '',
	status      TEXT NOT NULL DEFAULT 'draft',
	memory      TEXT NOT NULL DEFAULT '',
	created_at  INTEGER NOT NULL,
	updated_at  INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS task_labels (
	task_id INTEGER NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
	label   TEXT NOT NULL,
	PRIMARY KEY (task_id, label)
);
CREATE TABLE IF NOT EXISTS attachments (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	task_id    INTEGER NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
	name       TEXT NOT NULL,
	size       INTEGER NOT NULL,
	created_at INTEGER NOT NULL,
	UNIQUE (task_id, name)
);
CREATE TABLE IF NOT EXISTS runs (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	task_id    INTEGER NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
	number     INTEGER NOT NULL,
	status     TEXT NOT NULL,
	summary    TEXT NOT NULL DEFAULT '',
	started_at INTEGER NOT NULL,
	ended_at   INTEGER,
	UNIQUE (task_id, number)
);
`

// Open creates dir if needed and opens (or initialises) the database in it.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(filepath.Join(dir, "files"), 0o755); err != nil {
		return nil, err
	}

	dsn := "file:" + filepath.Join(dir, "tasks.db") + "?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)

	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("creating schema: %w", err)
	}

	return &Store{db: db, dir: dir}, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

// ListTasks returns every task, most recently updated first.
func (s *Store) ListTasks() ([]Task, error) {
	rows, err := s.db.Query(`
		SELECT t.id, t.title, t.description, t.category, t.status, t.memory, t.created_at, t.updated_at,
			(SELECT COUNT(*) FROM attachments a WHERE a.task_id = t.id),
			(SELECT COUNT(*) FROM runs r WHERE r.task_id = t.id)
		FROM tasks t
		ORDER BY t.updated_at DESC, t.id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var tasks []Task
	for rows.Next() {
		var t Task
		var created, updated int64
		err := rows.Scan(&t.ID, &t.Title, &t.Description, &t.Category, &t.Status, &t.Memory,
			&created, &updated, &t.FileCount, &t.RunCount)
		if err != nil {
			return nil, err
		}
		t.CreatedAt = time.Unix(created, 0)
		t.UpdatedAt = time.Unix(updated, 0)
		tasks = append(tasks, t)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	labels, err := s.labels(0)
	if err != nil {
		return nil, err
	}
	for i := range tasks {
		tasks[i].Labels = labels[tasks[i].ID]
	}

	return tasks, nil
}

func (s *Store) GetTask(id int64) (*Task, error) {
	var t Task
	var created, updated int64
	err := s.db.QueryRow(`
		SELECT id, title, description, category, status, memory, created_at, updated_at
		FROM tasks WHERE id = ?`, id).
		Scan(&t.ID, &t.Title, &t.Description, &t.Category, &t.Status, &t.Memory, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	t.CreatedAt = time.Unix(created, 0)
	t.UpdatedAt = time.Unix(updated, 0)

	labels, err := s.labels(id)
	if err != nil {
		return nil, err
	}
	t.Labels = labels[id]

	return &t, nil
}

// labels returns labels grouped by task id, in the order they were added.
// A taskID of 0 loads the labels of every task.
func (s *Store) labels(taskID int64) (map[int64][]string, error) {
	rows, err := s.db.Query(`
		SELECT task_id, label FROM task_labels
		WHERE ? = 0 OR task_id = ?
		ORDER BY rowid`, taskID, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	labels := map[int64][]string{}
	for rows.Next() {
		var id int64
		var label string
		if err := rows.Scan(&id, &label); err != nil {
			return nil, err
		}
		labels[id] = append(labels[id], label)
	}
	return labels, rows.Err()
}

// CreateTask stores a new draft task and returns its id.
func (s *Store) CreateTask(t *Task) (int64, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	now := time.Now().Unix()
	res, err := tx.Exec(`
		INSERT INTO tasks (title, description, category, status, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		t.Title, t.Description, t.Category, StatusDraft, now, now)
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}

	if err := replaceLabels(tx, id, t.Labels); err != nil {
		return 0, err
	}

	return id, tx.Commit()
}

// UpdateTask saves the editable fields of t: title, description, category and labels.
func (s *Store) UpdateTask(t *Task) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	res, err := tx.Exec(`
		UPDATE tasks SET title = ?, description = ?, category = ?, updated_at = ?
		WHERE id = ?`,
		t.Title, t.Description, t.Category, time.Now().Unix(), t.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}

	if err := replaceLabels(tx, t.ID, t.Labels); err != nil {
		return err
	}

	return tx.Commit()
}

func replaceLabels(tx *sql.Tx, taskID int64, labels []string) error {
	if _, err := tx.Exec(`DELETE FROM task_labels WHERE task_id = ?`, taskID); err != nil {
		return err
	}
	for _, label := range labels {
		_, err := tx.Exec(`INSERT OR IGNORE INTO task_labels (task_id, label) VALUES (?, ?)`, taskID, label)
		if err != nil {
			return err
		}
	}
	return nil
}

// SetMemory replaces the context the wrapper carries between agent runs of a task.
func (s *Store) SetMemory(taskID int64, memory string) error {
	res, err := s.db.Exec(`UPDATE tasks SET memory = ?, updated_at = ? WHERE id = ?`,
		memory, time.Now().Unix(), taskID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) ListAttachments(taskID int64) ([]Attachment, error) {
	rows, err := s.db.Query(`
		SELECT id, task_id, name, size, created_at FROM attachments
		WHERE task_id = ? ORDER BY id`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var files []Attachment
	for rows.Next() {
		var a Attachment
		var created int64
		if err := rows.Scan(&a.ID, &a.TaskID, &a.Name, &a.Size, &created); err != nil {
			return nil, err
		}
		a.CreatedAt = time.Unix(created, 0)
		files = append(files, a)
	}
	return files, rows.Err()
}

func (s *Store) GetAttachment(taskID, id int64) (*Attachment, error) {
	var a Attachment
	var created int64
	err := s.db.QueryRow(`
		SELECT id, task_id, name, size, created_at FROM attachments
		WHERE task_id = ? AND id = ?`, taskID, id).
		Scan(&a.ID, &a.TaskID, &a.Name, &a.Size, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	a.CreatedAt = time.Unix(created, 0)
	return &a, nil
}

// TaskDir is the directory holding the files attached to a task.
func (s *Store) TaskDir(taskID int64) string {
	return filepath.Join(s.dir, "files", fmt.Sprintf("%d", taskID))
}

// AttachmentPath is where the content of an attachment is stored on disk.
func (s *Store) AttachmentPath(a *Attachment) string {
	return filepath.Join(s.TaskDir(a.TaskID), a.Name)
}

// SaveAttachment writes r to the task's directory under name. A file already
// attached to the task under the same name is replaced.
func (s *Store) SaveAttachment(taskID int64, name string, r io.Reader) error {
	name = filepath.Base(name)
	if name == "." || name == ".." || name == string(filepath.Separator) {
		return fmt.Errorf("invalid file name %q", name)
	}

	dir := s.TaskDir(taskID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	// Write to a temporary file first so a failed upload never leaves a partial attachment.
	tmp, err := os.CreateTemp(dir, ".upload-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())

	size, err := io.Copy(tmp, r)
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), filepath.Join(dir, name)); err != nil {
		return err
	}

	now := time.Now().Unix()
	_, err = s.db.Exec(`
		INSERT INTO attachments (task_id, name, size, created_at) VALUES (?, ?, ?, ?)
		ON CONFLICT (task_id, name) DO UPDATE SET size = excluded.size, created_at = excluded.created_at`,
		taskID, name, size, now)
	if err != nil {
		return err
	}

	_, err = s.db.Exec(`UPDATE tasks SET updated_at = ? WHERE id = ?`, now, taskID)
	return err
}

func (s *Store) DeleteAttachment(taskID, id int64) error {
	a, err := s.GetAttachment(taskID, id)
	if err != nil {
		return err
	}

	if _, err := s.db.Exec(`DELETE FROM attachments WHERE id = ?`, id); err != nil {
		return err
	}
	if err := os.Remove(s.AttachmentPath(a)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// ListRuns returns the agent runs of a task, newest first.
func (s *Store) ListRuns(taskID int64) ([]Run, error) {
	rows, err := s.db.Query(`
		SELECT id, task_id, number, status, summary, started_at, ended_at FROM runs
		WHERE task_id = ? ORDER BY number DESC`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var runs []Run
	for rows.Next() {
		var r Run
		var started int64
		var ended sql.NullInt64
		if err := rows.Scan(&r.ID, &r.TaskID, &r.Number, &r.Status, &r.Summary, &started, &ended); err != nil {
			return nil, err
		}
		r.StartedAt = time.Unix(started, 0)
		if ended.Valid {
			t := time.Unix(ended.Int64, 0)
			r.EndedAt = &t
		}
		runs = append(runs, r)
	}
	return runs, rows.Err()
}

// QueueRun records a new run for the task in the queued state and marks the
// task as queued. Nothing here talks to herdr: picking up queued runs and
// moving them through running/done/failed is the dispatcher's job.
func (s *Store) QueueRun(taskID int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	now := time.Now().Unix()
	res, err := tx.Exec(`UPDATE tasks SET status = ?, updated_at = ? WHERE id = ?`, StatusQueued, now, taskID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}

	_, err = tx.Exec(`
		INSERT INTO runs (task_id, number, status, started_at)
		VALUES (?, (SELECT COALESCE(MAX(number), 0) + 1 FROM runs WHERE task_id = ?), ?, ?)`,
		taskID, taskID, StatusQueued, now)
	if err != nil {
		return err
	}

	return tx.Commit()
}
