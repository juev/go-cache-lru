package cache

import (
	"database/sql"
	"fmt"
	"net/url"
	"sync"
	"time"

	_ "modernc.org/sqlite" // database/sql driver "sqlite"
)

// flushInterval is how long the first queued change waits for others to
// join its batch. A crash loses at most this much of the index; the next
// start reconciles the table with the objects on disk, so nothing is
// corrupted.
const flushInterval = 100 * time.Millisecond

const schema = `
CREATE TABLE IF NOT EXISTS entries (
	action   TEXT PRIMARY KEY,
	output   TEXT NOT NULL,
	size     INTEGER NOT NULL,
	put_time INTEGER NOT NULL, -- unix nanos
	atime    INTEGER NOT NULL  -- unix nanos, last access (LRU key)
) WITHOUT ROWID;
`

type opKind int

const (
	opUpsert opKind = iota
	opTouch
	opDelete
)

type op struct {
	kind    opKind
	action  string
	output  string
	size    int64
	putTime int64
	atime   int64
}

// row is a stored index entry.
type row struct {
	action, output string
	size           int64
	putTime, atime time.Time
}

// index persists the in-memory index to SQLite. Changes are queued in
// order and written in batches by a background goroutine, so the cache's
// hot path never waits for the database. The goroutine sleeps until a
// change arrives: an idle daemon does not wake up for the index.
type index struct {
	db   *sql.DB
	logf func(string, ...any)

	mu      sync.Mutex
	pending []op
	wake    chan struct{} // a change arrived in an empty queue

	flushMu   sync.Mutex // serializes flushes to keep op order
	stop      chan struct{}
	done      chan struct{}
	closeOnce sync.Once
	closeErr  error
}

func openIndex(path string, logf func(string, ...any)) (*index, error) {
	q := url.Values{}
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "synchronous(NORMAL)")
	q.Add("_pragma", "busy_timeout(10000)")
	// url.URL escapes "?", "#" and "%" in the path, which SQLite would
	// otherwise parse as URI syntax.
	dsn := (&url.URL{Scheme: "file", OmitHost: true, Path: path, RawQuery: q.Encode()}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("index %s: %w", path, err)
	}
	x := &index{db: db, logf: logf, wake: make(chan struct{}, 1), stop: make(chan struct{}), done: make(chan struct{})}
	go x.loop()
	return x, nil
}

func (x *index) add(o op) {
	x.mu.Lock()
	first := len(x.pending) == 0
	x.pending = append(x.pending, o)
	x.mu.Unlock()
	if first {
		x.signal()
	}
}

func (x *index) signal() {
	select {
	case x.wake <- struct{}{}:
	default:
	}
}

func (x *index) upsert(action, output string, size int64, put time.Time) {
	n := put.UnixNano()
	x.add(op{kind: opUpsert, action: action, output: output, size: size, putTime: n, atime: n})
}

func (x *index) touch(action string, atime time.Time) {
	x.add(op{kind: opTouch, action: action, atime: atime.UnixNano()})
}

func (x *index) delete(action string) {
	x.add(op{kind: opDelete, action: action})
}

func (x *index) loop() {
	defer close(x.done)
	t := time.NewTimer(flushInterval)
	t.Stop()
	for {
		select {
		case <-x.wake:
		case <-x.stop:
			return
		}
		// Let the rest of the burst join the batch.
		t.Reset(flushInterval)
		select {
		case <-t.C:
		case <-x.stop:
			t.Stop()
			return
		}
		if err := x.flush(); err != nil {
			x.logf("index flush: %v", err)
			// The batch is queued again; retry after another interval.
			x.signal()
		}
	}
}

// flush writes the queued changes in one transaction.
func (x *index) flush() error {
	x.flushMu.Lock()
	defer x.flushMu.Unlock()
	x.mu.Lock()
	ops := x.pending
	x.pending = nil
	x.mu.Unlock()
	if len(ops) == 0 {
		return nil
	}
	if err := x.apply(ops); err != nil {
		// Keep the batch, ahead of newer changes, for the next flush.
		x.mu.Lock()
		x.pending = append(ops, x.pending...)
		x.mu.Unlock()
		return err
	}
	return nil
}

func (x *index) apply(ops []op) error {
	tx, err := x.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	upsert, err := tx.Prepare(`INSERT INTO entries (action, output, size, put_time, atime) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (action) DO UPDATE SET output = excluded.output, size = excluded.size,
		put_time = excluded.put_time, atime = excluded.atime`)
	if err != nil {
		return err
	}
	touch, err := tx.Prepare(`UPDATE entries SET atime = ? WHERE action = ?`)
	if err != nil {
		return err
	}
	del, err := tx.Prepare(`DELETE FROM entries WHERE action = ?`)
	if err != nil {
		return err
	}
	for _, o := range ops {
		switch o.kind {
		case opUpsert:
			_, err = upsert.Exec(o.action, o.output, o.size, o.putTime, o.atime)
		case opTouch:
			_, err = touch.Exec(o.atime, o.action)
		case opDelete:
			_, err = del.Exec(o.action)
		}
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

// load returns all stored entries, oldest access first.
func (x *index) load() ([]row, error) {
	rows, err := x.db.Query(`SELECT action, output, size, put_time, atime FROM entries ORDER BY atime`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var res []row
	for rows.Next() {
		var r row
		var put, atime int64
		if err := rows.Scan(&r.action, &r.output, &r.size, &put, &atime); err != nil {
			return nil, err
		}
		r.putTime, r.atime = time.Unix(0, put), time.Unix(0, atime)
		res = append(res, r)
	}
	return res, rows.Err()
}

// close flushes pending changes and closes the database.
func (x *index) close() error {
	x.closeOnce.Do(func() {
		close(x.stop)
		<-x.done
		x.closeErr = x.flush()
		if err := x.db.Close(); x.closeErr == nil {
			x.closeErr = err
		}
	})
	return x.closeErr
}
