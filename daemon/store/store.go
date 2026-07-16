// Package store is the daemon's durable key/value persistence, backed by bbolt
// (pure Go, no cgo). It is the single embedded store for state that must survive
// a restart: the issuer key, the revocation set, and (next) the ledger and CRDT
// checkpoints. Buckets namespace different subsystems.
package store

import (
	"errors"

	bolt "go.etcd.io/bbolt"
)

// ErrClosed is returned when operating on a closed store.
var ErrClosed = errors.New("store is closed")

// Store is a durable bucketed key/value store.
type Store struct {
	db *bolt.DB
}

// Open opens (or creates) the store at path.
func Open(path string) (*Store, error) {
	db, err := bolt.Open(path, 0o600, nil)
	if err != nil {
		return nil, err
	}
	return &Store{db: db}, nil
}

// Path reports the file this store was opened at. Callers use it to place
// sibling durable state in the same directory (daemon/system derives the /cer/fs
// shard directory from it) without threading a second path parameter through
// every constructor.
func (s *Store) Path() string {
	if s.db == nil {
		return ""
	}
	return s.db.Path()
}

// Close flushes and closes the store.
func (s *Store) Close() error {
	if s.db == nil {
		return nil
	}
	return s.db.Close()
}

// Put writes a value under bucket/key, creating the bucket if needed.
func (s *Store) Put(bucket, key string, val []byte) error {
	if s.db == nil {
		return ErrClosed
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists([]byte(bucket))
		if err != nil {
			return err
		}
		return b.Put([]byte(key), val)
	})
}

// Get reads bucket/key. The bool reports whether the key exists.
func (s *Store) Get(bucket, key string) ([]byte, bool, error) {
	if s.db == nil {
		return nil, false, ErrClosed
	}
	var out []byte
	var found bool
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(bucket))
		if b == nil {
			return nil
		}
		v := b.Get([]byte(key))
		if v != nil {
			out = append([]byte(nil), v...) // copy; bolt bytes are only valid in-tx
			found = true
		}
		return nil
	})
	return out, found, err
}

// Delete removes bucket/key (no error if absent).
func (s *Store) Delete(bucket, key string) error {
	if s.db == nil {
		return ErrClosed
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(bucket))
		if b == nil {
			return nil
		}
		return b.Delete([]byte(key))
	})
}

// Keys lists all keys in a bucket (empty if the bucket does not exist).
func (s *Store) Keys(bucket string) ([]string, error) {
	if s.db == nil {
		return nil, ErrClosed
	}
	var keys []string
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(bucket))
		if b == nil {
			return nil
		}
		return b.ForEach(func(k, _ []byte) error {
			keys = append(keys, string(k))
			return nil
		})
	})
	return keys, err
}
