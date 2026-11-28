package relay

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/citywriteauth"
	"github.com/gastownhall/gascity/internal/mailrelay/envelope"
	_ "modernc.org/sqlite" // Register the pure Go SQLite driver.
)

const (
	retention     = 30 * 24 * time.Hour
	batchCount    = 32
	batchBytes    = 2 * envelope.MaxEnvelopeSize
	queueCapacity = 1024
	maxGrantID    = 128
)

var (
	errConflict  = errors.New("conflicting message id")
	errRate      = errors.New("post rate exceeded")
	errQueueFull = errors.New("recipient queue full")
	errCursor    = errors.New("cursor exceeds queue number")
)

// Delivery carries one sealed wire envelope and its recipient-local queue number.
type Delivery struct {
	Number   int64  `json:"number"`
	Envelope []byte `json:"envelope"`
}

type store struct{ db *sql.DB }

// Labels survive acknowledgement, allowing a sender to retry a lost response.
// Queue counters survive both acknowledgement and expiry.
const schema = `
CREATE TABLE IF NOT EXISTS queues(city TEXT PRIMARY KEY, last INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS labels(
 id BLOB PRIMARY KEY, sender TEXT NOT NULL, recipient TEXT NOT NULL,
 sent INTEGER NOT NULL, size INTEGER NOT NULL, received INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS labels_received ON labels(received);
CREATE TABLE IF NOT EXISTS envelopes(
 city TEXT NOT NULL, number INTEGER NOT NULL, wire BLOB NOT NULL, received INTEGER NOT NULL,
 PRIMARY KEY(city, number)
);
CREATE INDEX IF NOT EXISTS envelopes_received ON envelopes(received);
CREATE TABLE IF NOT EXISTS statements(city TEXT PRIMARY KEY, wire BLOB NOT NULL);
CREATE TABLE IF NOT EXISTS grants(id TEXT PRIMARY KEY, expires INTEGER NOT NULL);
CREATE INDEX IF NOT EXISTS grants_expiry ON grants(expires);
CREATE TABLE IF NOT EXISTS rates(city TEXT PRIMARY KEY, minute INTEGER NOT NULL, posts INTEGER NOT NULL);
`

func openStore(path string) (*store, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	file, err := os.OpenFile(abs, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := file.Close(); err != nil {
		return nil, err
	}
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(abs)}
	q := u.Query()
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "secure_delete(ON)")
	q.Set("_txlock", "immediate")
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		return nil, errors.Join(err, db.Close())
	}
	return &store{db: db}, nil
}

func (s *store) transaction(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // Commit or a prior error determines the result.
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// Use atomically consumes a verified city's grant ID through its last valid instant.
func (s *store) Use(id string, expiry time.Time) error {
	_, jti, found := strings.Cut(id, "/")
	if !found || len(jti) == 0 || len(jti) > maxGrantID {
		return citywriteauth.ErrMalformed
	}
	// The writer may have waited for a connection since signature verification.
	// At insertion the ID must still be inside the same window used by the sweep.
	result, err := s.db.Exec(`INSERT INTO grants(id,expires)
SELECT ?, ? WHERE unixepoch() <= ? ON CONFLICT(id) DO NOTHING`, id, expiry.Unix(), expiry.Unix())
	if err != nil {
		return fmt.Errorf("record used grant: %w", err)
	}
	n, err := result.RowsAffected()
	if err == nil && n == 0 {
		return citywriteauth.ErrReplay
	}
	return err
}

func (s *store) post(ctx context.Context, label envelope.Label, wire []byte, rate int, now time.Time) error {
	return s.transaction(ctx, func(tx *sql.Tx) error {
		var sender, recipient string
		err := tx.QueryRowContext(ctx, "SELECT sender, recipient FROM labels WHERE id=?", []byte(label.ID)).Scan(&sender, &recipient)
		switch {
		case err == nil && sender == label.From && recipient == label.To:
			return nil
		case err == nil:
			return errConflict
		case !errors.Is(err, sql.ErrNoRows):
			return err
		}
		var used int
		if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM envelopes WHERE city=?", label.To).Scan(&used); err != nil {
			return err
		}
		if used >= queueCapacity {
			return errQueueFull
		}
		minute := now.Unix() / 60
		result, err := tx.ExecContext(ctx, `INSERT INTO rates VALUES(?,?,1)
ON CONFLICT(city) DO UPDATE SET minute=excluded.minute,
posts=CASE WHEN rates.minute=excluded.minute THEN posts+1 ELSE 1 END
WHERE rates.minute<>excluded.minute OR posts<?`, label.From, minute, rate)
		if err != nil {
			return err
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if changed == 0 {
			return errRate
		}
		var number int64
		err = tx.QueryRowContext(ctx, `INSERT INTO queues VALUES(?,1)
ON CONFLICT(city) DO UPDATE SET last=last+1 RETURNING last`, label.To).Scan(&number)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO labels VALUES(?,?,?,?,?,?)", []byte(label.ID), label.From, label.To, label.Sent, len(wire), now.Unix()); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, "INSERT INTO envelopes VALUES(?,?,?,?)", label.To, number, wire, now.Unix())
		return err
	})
}

func (s *store) acknowledge(ctx context.Context, city string, after int64) error {
	if after == 0 {
		return nil
	}
	return s.transaction(ctx, func(tx *sql.Tx) error {
		var last int64
		err := tx.QueryRowContext(ctx, "SELECT last FROM queues WHERE city=?", city).Scan(&last)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if after > last {
			return errCursor
		}
		_, err = tx.ExecContext(ctx, "DELETE FROM envelopes WHERE city=? AND number<=?", city, after)
		return err
	})
}

func (s *store) read(ctx context.Context, city string, after int64) ([]Delivery, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT number, wire FROM envelopes WHERE city=? AND number>? ORDER BY number LIMIT ?", city, after, batchCount)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck // rows.Err reports read errors.
	batch := make([]Delivery, 0)
	size := 0
	for rows.Next() {
		var d Delivery
		if err := rows.Scan(&d.Number, &d.Envelope); err != nil {
			return nil, err
		}
		size += len(d.Envelope)
		if size > batchBytes {
			break
		}
		batch = append(batch, d)
	}
	return batch, rows.Err()
}

// publish stores a city's key statement, already verified under signer, the
// city's current signing key. A stored statement that signer did not sign was
// made under a key the city no longer holds, so it is replaced, not compared:
// a retired key cannot hold back the city's next statement.
func (s *store) publish(ctx context.Context, next envelope.KeyStatement, wire []byte, signer ed25519.PublicKey) error {
	return s.transaction(ctx, func(tx *sql.Tx) error {
		var prev []byte
		err := tx.QueryRowContext(ctx, "SELECT wire FROM statements WHERE city=?", next.City).Scan(&prev)
		if err == nil {
			old, err := envelope.ParseStatement(prev)
			if err != nil {
				return err
			}
			if old.Verify(next.City, signer) == nil {
				if err := old.CheckSuccessor(next); err != nil {
					return err
				}
			}
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		_, err = tx.ExecContext(ctx, "INSERT INTO statements VALUES(?,?) ON CONFLICT(city) DO UPDATE SET wire=excluded.wire", next.City, wire)
		return err
	})
}

func (s *store) sweep(ctx context.Context, now time.Time) error {
	cutoff := now.Add(-retention).Unix()
	return s.transaction(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "DELETE FROM envelopes WHERE received<=?", cutoff); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM labels WHERE received<=?", cutoff); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, "DELETE FROM grants WHERE expires<?", now.Unix())
		return err
	})
}
