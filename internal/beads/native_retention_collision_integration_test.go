//go:build integration

package beads

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	beadslib "github.com/steveyegge/beads"
)

func TestNativeExactPagesCanonicalTiersAgainstRealDolt(t *testing.T) {
	port := startTestDoltServer(t)
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "BEADS_") {
			t.Setenv(key, "")
		}
	}
	for key, value := range map[string]string{
		"BEADS_DOLT_SERVER_HOST": "127.0.0.1", "BEADS_DOLT_SERVER_PORT": strconv.Itoa(port),
		"BEADS_DOLT_SERVER_DATABASE": "beads", "BEADS_DOLT_SERVER_USER": "root",
		"BEADS_DOLT_AUTO_START": "0", "BEADS_DOLT_SERVER_MODE": "1",
	} {
		t.Setenv(key, value)
	}
	dir := filepath.Join(t.TempDir(), ".beads")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	metadata := fmt.Sprintf(`{"backend":"dolt","database":"beads","dolt_database":"beads","dolt_mode":"server","dolt_server_host":"127.0.0.1","dolt_server_port":%d}`, port)
	if err := os.WriteFile(filepath.Join(dir, "metadata.json"), []byte(metadata), 0o600); err != nil {
		t.Fatal(err)
	}
	storage, err := beadslib.OpenFromConfig(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := storage.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := storage.SetConfig(context.Background(), "issue_prefix", "page"); err != nil {
		t.Fatal(err)
	}
	store := newNativeDoltStoreWithStorageAndPrefix(storage, "canonical-page-test", "page")
	seed, err := store.Create(Bead{Title: "schema seed", Ephemeral: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(seed.ID); err != nil {
		t.Fatal(err)
	}
	db := storage.(testRawDBGetter).DB()
	now := time.Date(2026, 1, 31, 0, 0, 0, 0, time.UTC)
	insert := func(table, labelTable, id, status string, created, updated time.Time, meta string) {
		t.Helper()
		if table == "issues" {
			if _, err := store.Create(Bead{ID: id, Title: "order:sample", Status: status, Type: "task", Labels: []string{"order-tracking", "order-run:sample"}}); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec("UPDATE issues SET created_at=?, updated_at=?, metadata=? WHERE id=?", created, updated, meta, id); err != nil {
				t.Fatal(err)
			}
			return
		}
		_, err := db.Exec("INSERT INTO "+table+" (id,title,status,priority,issue_type,created_at,updated_at,ephemeral,metadata) VALUES (?,?,?,2,'task',?,?,?,?)",
			id, "order:sample", status, created, updated, table == "wisps", meta)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec("INSERT INTO "+labelTable+" (issue_id,label) VALUES (?, 'order-tracking'), (?, 'order-run:sample')", id, id); err != nil {
			t.Fatal(err)
		}
	}
	insert("issues", "labels", "page-duplicate", "closed", now.Add(-20*24*time.Hour), now.Add(-20*24*time.Hour), "{}")
	insert("wisps", "wisp_labels", "page-duplicate", "closed", now.Add(-30*24*time.Hour), now.Add(-time.Hour), "{}")
	for i := range 2 {
		insert("wisps", "wisp_labels", fmt.Sprintf("page-run-%04d", i), "closed", now.Add(-21*24*time.Hour), now.Add(-10*24*time.Hour), "{}")
	}
	t.Run("retention_timestamp", func(t *testing.T) {
		q := ListQuery{Status: "closed", Label: "order-tracking", Sort: SortCreatedDesc, TierMode: TierBoth}
		whole, err := store.List(q)
		if err != nil {
			t.Fatal(err)
		}
		want := make(map[string]Bead, len(whole))
		for _, b := range whole {
			want[b.ID] = b
		}
		if !want["page-duplicate"].CreatedAt.Equal(now.Add(-30 * 24 * time.Hour)) {
			t.Fatal("unbounded control did not select canonical wisp")
		}
		q.Sort, q.Limit = SortCreatedDescIDAsc, 2
		seen := map[string]bool{}
		for {
			page, err := store.List(q)
			if err != nil {
				t.Fatal(err)
			}
			for _, b := range page {
				if seen[b.ID] || !b.CreatedAt.Equal(want[b.ID].CreatedAt) {
					t.Fatalf("noncanonical or duplicate paged row %s: created=%s want=%s", b.ID, b.CreatedAt, want[b.ID].CreatedAt)
				}
				seen[b.ID] = true
			}
			if len(page) < q.Limit {
				break
			}
			last := page[len(page)-1]
			q.SeekAfter = &SeekBoundary{CreatedAt: last.CreatedAt, ID: last.ID}
		}
		if len(seen) != len(want) {
			t.Fatalf("paged %d rows, want %d", len(seen), len(want))
		}
	})
	t.Run("closed_canonical_member", func(t *testing.T) {
		meta := `{"gc.root_bead_id":"scope"}`
		insert("issues", "labels", "page-shadow", "open", now, now, meta)
		insert("wisps", "wisp_labels", "page-shadow", "closed", now, now, meta)
		q := ListQuery{Metadata: map[string]string{"gc.root_bead_id": "scope"}, TierMode: TierBoth, Sort: SortCreatedDescIDAsc, Limit: 2}
		rows, err := store.List(q)
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 0 {
			t.Fatalf("stale durable twin appeared as live work: %v", rows)
		}
	})
}
