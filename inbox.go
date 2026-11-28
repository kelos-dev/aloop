// Package aloop runs a single tool-using agent over a durable inbox of signal summaries.
package aloop

import (
	"encoding/json"
	"errors"
	"sync"
	"time"

	bolt "go.etcd.io/bbolt"
)

// Entry identifies an occurrence. Ref locates the original data in its source.
type Entry struct {
	ID      string `json:"id"`
	Source  string `json:"source"`
	Ref     string `json:"ref"`
	Summary string `json:"summary"`
}

// PendingSignal is an inbox entry with its persisted deferral state.
type PendingSignal struct {
	Entry
	Deferred bool
	Until    time.Time
}

type Inbox struct {
	db    *bolt.DB
	wake  chan struct{}
	runMu sync.Mutex
}

var entries = []byte("pending")
var seen = []byte("received")
var receipts = []byte("receipts")
var cursors = []byte("cursors")

func OpenInbox(path string) (*Inbox, error) {
	db, err := bolt.Open(path, 0600, &bolt.Options{Timeout: time.Second})
	if err != nil {
		return nil, err
	}
	err = db.Update(func(tx *bolt.Tx) error {
		for _, key := range [][]byte{entries, seen, receipts, cursors} {
			if _, err := tx.CreateBucketIfNotExists(key); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		db.Close()
		return nil, err
	}
	return &Inbox{db: db, wake: make(chan struct{}, 1)}, nil
}

func (i *Inbox) Close() error { return i.db.Close() }

// Received includes handled entries so a connector can cheaply skip a replay.
func (i *Inbox) Received(id string) (bool, error) {
	var found bool
	err := i.db.View(func(tx *bolt.Tx) error { found = tx.Bucket(seen).Get([]byte(id)) != nil; return nil })
	return found, err
}

// Append commits the batch before waking the scheduler. IDs are unique across sources.
func (i *Inbox) Append(batch ...Entry) error {
	return i.append("", "", batch)
}

// Cursor returns a source's last committed position, or an empty string on first use.
func (i *Inbox) Cursor(source string) (string, error) {
	var cursor string
	err := i.db.View(func(tx *bolt.Tx) error {
		cursor = string(tx.Bucket(cursors).Get([]byte(source)))
		return nil
	})
	return cursor, err
}

// AppendWithCursor commits signals and their source position in the same transaction.
func (i *Inbox) AppendWithCursor(source, cursor string, batch ...Entry) error {
	if source == "" || cursor == "" {
		return errors.New("source and cursor required")
	}
	return i.append(source, cursor, batch)
}

func (i *Inbox) append(source, cursor string, batch []Entry) error {
	if len(batch) == 0 && source == "" {
		return nil
	}
	added := false
	err := i.db.Update(func(tx *bolt.Tx) error {
		for _, e := range batch {
			if e.ID == "" || e.Source == "" || e.Summary == "" {
				return errors.New("entry requires id, source and summary")
			}
			key := []byte(e.ID)
			if tx.Bucket(seen).Get(key) != nil {
				continue
			}
			data, err := json.Marshal(PendingSignal{Entry: e})
			if err != nil {
				return err
			}
			if err = tx.Bucket(entries).Put(key, data); err != nil {
				return err
			}
			if err = tx.Bucket(seen).Put(key, []byte{1}); err != nil {
				return err
			}
			added = true
		}
		if source != "" {
			return tx.Bucket(cursors).Put([]byte(source), []byte(cursor))
		}
		return nil
	})
	if err == nil && added {
		select {
		case i.wake <- struct{}{}:
		default:
		}
	}
	return err
}

func (i *Inbox) snapshot(now time.Time) ([]Entry, error) {
	var batch []Entry
	ready := false
	err := i.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(entries).ForEach(func(_, v []byte) error {
			var p PendingSignal
			if err := json.Unmarshal(v, &p); err != nil {
				return err
			}
			ready = ready || !p.Deferred || (!p.Until.IsZero() && !p.Until.After(now))
			batch = append(batch, p.Entry)
			return nil
		})
	})
	if !ready {
		return nil, err
	}
	return batch, err
}

type Deferral struct {
	ID    string     `json:"id"`
	Until *time.Time `json:"until,omitempty"`
}

func (i *Inbox) settle(snapshotTime time.Time, done []string, deferred []Deferral) error {
	return i.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(entries)
		for _, id := range done {
			if err := b.Delete([]byte(id)); err != nil {
				return err
			}
		}
		for _, d := range deferred {
			var p PendingSignal
			if err := json.Unmarshal(b.Get([]byte(d.ID)), &p); err != nil {
				return err
			}
			p.Deferred = true
			if d.Until != nil {
				if !d.Until.After(time.Now()) {
					return errors.New("deferral deadline must be in the future")
				}
				p.Until = *d.Until
			} else if !p.Until.After(snapshotTime) {
				// Only consume deadlines that were already due when this snapshot was taken.
				p.Until = time.Time{}
			}
			v, err := json.Marshal(p)
			if err != nil {
				return err
			}
			if err = b.Put([]byte(d.ID), v); err != nil {
				return err
			}
		}
		return nil
	})
}

// Receipt identifies a completed effect without storing email or task contents.
type Receipt struct {
	Entry        string `json:"entry"`
	Tool         string `json:"tool"`
	Result       string `json:"result"`
	ArgumentHash string `json:"argument_hash"`
}

func (i *Inbox) receipt(key string) (Receipt, bool, error) {
	var r Receipt
	var found bool
	err := i.db.View(func(tx *bolt.Tx) error {
		v := tx.Bucket(receipts).Get([]byte(key))
		if v == nil {
			return nil
		}
		found = true
		return json.Unmarshal(v, &r)
	})
	return r, found, err
}

func (i *Inbox) record(key string, r Receipt) error {
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return i.db.Update(func(tx *bolt.Tx) error { return tx.Bucket(receipts).Put([]byte(key), data) })
}

// InboxStatus describes the durable signals and completed effects in one read transaction.
type InboxStatus struct {
	Received int
	Pending  []PendingSignal
	Effects  map[string]int
}

// Status includes deferred signals even when none are eligible to wake the agent.
func (i *Inbox) Status() (InboxStatus, error) {
	status := InboxStatus{Effects: make(map[string]int)}
	err := i.db.View(func(tx *bolt.Tx) error {
		status.Received = tx.Bucket(seen).Stats().KeyN
		err := tx.Bucket(entries).ForEach(func(_, data []byte) error {
			var signal PendingSignal
			if err := json.Unmarshal(data, &signal); err != nil {
				return err
			}
			status.Pending = append(status.Pending, signal)
			return nil
		})
		if err != nil {
			return err
		}
		return tx.Bucket(receipts).ForEach(func(_, data []byte) error {
			var receipt Receipt
			if err := json.Unmarshal(data, &receipt); err != nil {
				return err
			}
			status.Effects[receipt.Tool]++
			return nil
		})
	})
	return status, err
}
