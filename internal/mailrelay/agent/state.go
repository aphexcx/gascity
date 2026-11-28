package agent

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// The files the agent keeps in its state folder.
const (
	placeFile = "place.json"
	seenFile  = "seen.json"
	peersDir  = "peers"
)

// seenFor is how long a filed mail's sender city and id are remembered. A
// mail opens only while it is younger than 30 days and at most 5 minutes
// ahead of the clock, so a copy arriving later can no longer open.
const seenFor = 30*24*time.Hour + 5*time.Minute

// filingKey names one mail by its sender city and id together: the
// Idempotency-Key the city's API files it under and the key it is remembered
// by. The length prefix keeps two different pairs apart and the hash keeps
// any id a sender picks a plain, fixed-length header value.
func filingKey(senderCity, id string) string {
	h := sha256.New()
	var n [2]byte
	binary.BigEndian.PutUint16(n[:], uint16(len(senderCity)))
	h.Write(n[:])
	h.Write([]byte(senderCity))
	h.Write([]byte(id))
	return "gc-mail-relay-v1-" + hex.EncodeToString(h.Sum(nil))
}

// inbound is the receiving side's durable state.
type inbound struct {
	dir   string
	owner placeOwner
	place int64
	seen  map[string]int64 // filing key -> Unix second first filed
}

// placeOwner is whose a saved place is. A place counts only for the city it
// was handed to, on the relay that handed out its numbers: anywhere else the
// relay would take it as an acknowledgement and delete that much unread.
type placeOwner struct {
	City  string
	Relay string
}

type placeRecord struct {
	Place int64  `json:"place"`
	City  string `json:"city,omitempty"`
	Relay string `json:"relay,omitempty"`
}

// ownerChange names why a saved place does not belong to owner, or is "".
func (p placeRecord) ownerChange(owner placeOwner) string {
	switch {
	case p.City == "" && p.Relay == "":
		return "owner-unrecorded"
	case p.City != owner.City:
		return "city-changed"
	case p.Relay != owner.Relay:
		return "relay-changed"
	}
	return ""
}

// placeReset is a saved place that was not taken: where it stood and why.
type placeReset struct {
	from   int64
	reason string
}

type seenRecord struct {
	Pairs map[string]int64 `json:"pairs"`
}

// loadInbound reads the agent's state for owner. A saved place that another
// city or relay owns, or that was saved without its owner, is not taken: the
// place starts from 0 and is saved for owner at once, and the reset is
// returned so the run can say so.
func loadInbound(dir string, owner placeOwner) (*inbound, *placeReset, error) {
	in := &inbound{dir: dir, owner: owner, seen: make(map[string]int64)}
	var p placeRecord
	saved, err := readJSON(filepath.Join(dir, placeFile), &p)
	if err != nil {
		return nil, nil, err
	}
	var s seenRecord
	if _, err := readJSON(filepath.Join(dir, seenFile), &s); err != nil {
		return nil, nil, err
	}
	if p.Place < 0 {
		return nil, nil, fmt.Errorf("state file %s holds a negative place", placeFile)
	}
	in.place = p.Place
	for k, v := range s.Pairs {
		in.seen[k] = v
	}
	if !saved {
		return in, nil, nil
	}
	reason := p.ownerChange(owner)
	if reason == "" {
		return in, nil, nil
	}
	if err := in.reset(); err != nil {
		return nil, nil, err
	}
	return in, &placeReset{from: p.Place, reason: reason}, nil
}

func (in *inbound) hasSeen(key string, now time.Time) bool {
	at, ok := in.seen[key]
	return ok && now.Sub(time.Unix(at, 0)) < seenFor
}

// remember records a filed mail and saves the list before anything else.
func (in *inbound) remember(key string, now time.Time) error {
	in.seen[key] = now.Unix()
	for k, at := range in.seen {
		if now.Sub(time.Unix(at, 0)) >= seenFor {
			delete(in.seen, k)
		}
	}
	return writeJSON(filepath.Join(in.dir, seenFile), seenRecord{Pairs: in.seen})
}

// advance saves the place. Only a number the relay delivered reaches here.
func (in *inbound) advance(number int64) error {
	if number <= in.place {
		return nil
	}
	if err := writeJSON(filepath.Join(in.dir, placeFile), in.record(number)); err != nil {
		return err
	}
	in.place = number
	return nil
}

// reset starts the place over from zero.
func (in *inbound) reset() error {
	if err := writeJSON(filepath.Join(in.dir, placeFile), in.record(0)); err != nil {
		return err
	}
	in.place = 0
	return nil
}

// record is the place as saved, with its owner.
func (in *inbound) record(place int64) placeRecord {
	return placeRecord{Place: place, City: in.owner.City, Relay: in.owner.Relay}
}

// readJSON fills v from path and reports whether the file was there; a
// missing file leaves v empty.
func readJSON(path string, v any) (bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("reading state: %w", err)
	}
	if err := json.Unmarshal(data, v); err != nil {
		return false, fmt.Errorf("state file %s: %w", path, err)
	}
	return true, nil
}

func writeJSON(path string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if err := writeFile(filepath.Dir(path), filepath.Base(path), data, privateFileMode, true); err != nil {
		return fmt.Errorf("saving state: %w", err)
	}
	return nil
}

// ensureStateDir makes the state folder and its peers folder, closed to the
// group and others.
func ensureStateDir(dir string) error {
	if err := os.MkdirAll(filepath.Join(dir, peersDir), privateDirMode); err != nil {
		return fmt.Errorf("making state folder: %w", err)
	}
	return nil
}

// peerStatement returns the newest key statement seen for a peer.
func peerStatement(stateDir, peer string) ([]byte, bool, error) {
	data, err := os.ReadFile(filepath.Join(stateDir, peersDir, peer+statementSuffix))
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("reading peer key statement: %w", err)
	}
	wire, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(data)))
	if err != nil {
		return nil, false, fmt.Errorf("peer key statement for %s: %w", peer, err)
	}
	return wire, true, nil
}

func savePeerStatement(stateDir, peer string, wire []byte) error {
	if err := writeFile(filepath.Join(stateDir, peersDir), peer+statementSuffix, encodeLine(wire), privateFileMode, true); err != nil {
		return fmt.Errorf("saving peer key statement: %w", err)
	}
	return nil
}

// writeNewFile creates dir/name durably and never replaces an existing file.
func writeNewFile(dir, name string, data []byte, perm os.FileMode) error {
	return writeFile(dir, name, data, perm, false)
}

// writeFile writes dir/name through a synced temporary file, so a crash
// leaves the old content or the new, never a torn file. With replace it
// renames over an existing file; without, a hard link names the new file and
// an existing one is never replaced.
func writeFile(dir, name string, data []byte, perm os.FileMode, replace bool) (err error) {
	final := filepath.Join(dir, name)
	tmp, err := os.CreateTemp(dir, "."+name+".tmp-*")
	if err != nil {
		return fmt.Errorf("writing %s: %w", name, err)
	}
	defer func() {
		if rmErr := os.Remove(tmp.Name()); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) && err == nil {
			err = fmt.Errorf("removing temporary %s: %w", name, rmErr)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("writing %s: %w", name, err)
	}
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("writing %s: %w", name, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("writing %s: %w", name, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("writing %s: %w", name, err)
	}
	if replace {
		if err := os.Rename(tmp.Name(), final); err != nil {
			return fmt.Errorf("writing %s: %w", name, err)
		}
	} else if err := os.Link(tmp.Name(), final); err != nil {
		return fmt.Errorf("refusing to replace %s: %w", final, err)
	}
	return syncDir(dir)
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close() //nolint:errcheck // Read-only directory handle.
	return d.Sync()
}
