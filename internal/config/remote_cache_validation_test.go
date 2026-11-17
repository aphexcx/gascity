package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const validationTestSource = "https://example.invalid/pack.git"

func validationFixture(t *testing.T) (string, *int, *string) {
	t.Helper()
	_, dir := setupLockedPackRefTest(t, validationTestSource)
	writeTestFile(t, dir, ".git/HEAD", lockedPackRefCommit)
	calls := 0
	status := ""
	old := runRepoCacheGit
	runRepoCacheGit = func(_ string, args ...string) (string, error) {
		calls++
		if args[0] == "rev-parse" {
			return lockedPackRefCommit, nil
		}
		return status, nil
	}
	t.Cleanup(func() { runRepoCacheGit = old })
	return dir, &calls, &status
}

func validateFixture(dir, commit string) error {
	return validateInstalledRemoteCacheLocked(validationTestSource, filepath.Dir(dir), dir, commit, true)
}

func TestRemoteCacheValidationFreshness(t *testing.T) {
	now := time.Date(2026, time.November, 16, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		age  time.Duration
		want bool
	}{
		{-time.Second, false}, {0, true}, {time.Minute - time.Nanosecond, true}, {time.Minute, false}, {2 * time.Minute, false},
	} {
		if got := remoteCacheValidationFresh(now.Add(-test.age), now); got != test.want {
			t.Errorf("age %s: fresh=%t, want %t", test.age, got, test.want)
		}
	}
}

func TestRemoteCacheValidationMalformedReceiptRechecks(t *testing.T) {
	dir, calls, _ := validationFixture(t)
	if err := validateFixture(dir, lockedPackRefCommit); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, dir, ".git/gc-validation-v1.json", "partial JSON")
	if err := validateFixture(dir, lockedPackRefCommit); err != nil || *calls != 4 {
		t.Fatalf("malformed receipt: calls=%d err=%v", *calls, err)
	}
}

func TestRemoteCacheValidationDoesNotRememberConcurrentEdit(t *testing.T) {
	dir, _, _ := validationFixture(t)
	calls := 0
	runRepoCacheGit = func(_ string, args ...string) (string, error) {
		calls++
		if args[0] == "rev-parse" {
			return lockedPackRefCommit, nil
		}
		writeTestFile(t, dir, "concurrent-edit", "changed during validation")
		return "", nil
	}
	if err := validateFixture(dir, lockedPackRefCommit); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(remoteCacheValidationPath(dir)); !os.IsNotExist(err) {
		t.Fatalf("concurrent edit was published as clean: %v", err)
	}
	if err := validateFixture(dir, lockedPackRefCommit); err != nil || calls != 4 {
		t.Fatalf("concurrent edit was memoized: calls=%d err=%v", calls, err)
	}
}

func TestRemoteCacheValidationSurvivesMemoryReset(t *testing.T) {
	dir, calls, _ := validationFixture(t)
	if err := validateFixture(dir, lockedPackRefCommit); err != nil {
		t.Fatal(err)
	}
	if *calls != 2 {
		t.Fatalf("cold validation: %d git calls, want 2", *calls)
	}
	ResetRemoteCacheValidationCache()
	if err := validateFixture(dir, lockedPackRefCommit); err != nil {
		t.Fatal(err)
	}
	if *calls != 2 {
		t.Fatalf("new process repeated validation: %d total git calls, want 2", *calls)
	}
}

func TestRemoteCacheValidationInvalidationRejectsChangesAndRetriesRepair(t *testing.T) {
	for _, changed := range []string{"root", "index", "HEAD", "receipt", "pin"} {
		t.Run(changed, func(t *testing.T) {
			dir, calls, status := validationFixture(t)
			if err := validateFixture(dir, lockedPackRefCommit); err != nil {
				t.Fatal(err)
			}
			commit := lockedPackRefCommit
			switch changed {
			case "root":
				writeTestFile(t, dir, "new-file", "local change")
			case "index", "HEAD":
				writeTestFile(t, dir, ".git/"+changed, "changed cached checkout")
			case "receipt":
				if err := os.Remove(filepath.Join(dir, ".git/gc-validation-v1.json")); err != nil {
					t.Fatal(err)
				}
			case "pin":
				commit = strings.Repeat("f", 40)
			}
			*status = " M nested/file"
			err := validateFixture(dir, commit)
			if err == nil || *calls <= 2 {
				t.Fatalf("%s invalidation accepted changed checkout: calls=%d err=%v", changed, *calls, err)
			}
			want := "local worktree changes"
			if changed == "pin" {
				want = "expected " + commit
			}
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("error %q does not contain %q", err, want)
			}
			failedCalls := *calls
			*status = ""
			if err := validateFixture(dir, lockedPackRefCommit); err != nil {
				t.Fatalf("repaired cache: %v", err)
			}
			if changed != "pin" && *calls != failedCalls+2 {
				t.Fatalf("failed check remembered: calls=%d, want %d", *calls, failedCalls+2)
			}
		})
	}
}

func TestRemoteCacheValidationBoundsNestedEditBlindSpot(t *testing.T) {
	dir, calls, status := validationFixture(t)
	writeTestFile(t, dir, "nested/file", "original")
	if err := validateFixture(dir, lockedPackRefCommit); err != nil {
		t.Fatal(err)
	}
	fp := remoteCacheFingerprint(dir)
	writeTestFile(t, dir, "nested/file", "local edit")
	if got := remoteCacheFingerprint(dir); got != fp {
		t.Fatal("fixture must leave the cheap fingerprint unchanged")
	}
	*status = " M nested/file"
	ResetRemoteCacheValidationCache()
	if err := validateFixture(dir, lockedPackRefCommit); err != nil || *calls != 2 {
		t.Fatalf("warm receipt: calls=%d err=%v", *calls, err)
	}
	path := filepath.Join(dir, ".git/gc-validation-v1.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var receipt map[string]any
	if err := json.Unmarshal(data, &receipt); err != nil {
		t.Fatal(err)
	}
	receipt["checked_at"] = time.Now().Add(-2 * time.Minute).UTC().Format(time.RFC3339Nano)
	data, err = json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validateFixture(dir, lockedPackRefCommit); err == nil || !strings.Contains(err.Error(), "local worktree changes") {
		t.Fatalf("expired receipt must detect nested edit: %v", err)
	}
	if *calls != 4 {
		t.Fatalf("expired receipt: %d git calls, want 4", *calls)
	}
}
