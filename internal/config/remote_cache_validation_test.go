package config

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/cacheexec"
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

func TestRemoteCacheReceiptAnswersFullDemandOnlyUnderItsConditions(t *testing.T) {
	t.Run("quick_never_writes_full_receipt", func(t *testing.T) {
		dir, calls, status := validationFixture(t)
		if err := validateInstalledRemoteCacheLocked(validationTestSource, filepath.Dir(dir), dir, lockedPackRefCommit, true, CacheValidationQuick); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(remoteCacheValidationPath(dir)); !os.IsNotExist(err) {
			t.Fatalf("QUICK wrote a receipt: %v", err)
		}
		*status = " M nested/file"
		if err := validateFixture(dir, lockedPackRefCommit); err == nil || !strings.Contains(err.Error(), "local worktree changes") || *calls != 2 {
			t.Fatalf("FULL after QUICK: calls=%d err=%v", *calls, err)
		}
	})
	t.Run("receipt_names_locked_commit", func(t *testing.T) {
		dir, calls, status := validationFixture(t)
		if err := validateFixture(dir, lockedPackRefCommit); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(remoteCacheValidationPath(dir))
		if err != nil {
			t.Fatal(err)
		}
		var receipt remoteCacheValidationRecord
		if err := json.Unmarshal(data, &receipt); err != nil {
			t.Fatal(err)
		}
		receipt.Commit = strings.Repeat("f", 40)
		data, err = json.Marshal(receipt)
		if err != nil {
			t.Fatal(err)
		}
		writeTestFile(t, dir, ".git/"+remoteCacheValidationFile, string(data))
		ResetRemoteCacheValidationCache()
		*status = " M nested/file"
		if err := validateFixture(dir, lockedPackRefCommit); err == nil || !strings.Contains(err.Error(), "local worktree changes") || *calls != 4 {
			t.Fatalf("receipt for other commit: calls=%d err=%v", *calls, err)
		}
	})
	for _, reload := range []bool{false, true} {
		name := map[bool]string{false: "reset_honors_receipt", true: "reload_refuses_receipt"}[reload]
		t.Run(name, func(t *testing.T) {
			dir, calls, status := validationFixture(t)
			writeTestFile(t, dir, "nested/file", "original")
			if err := validateFixture(dir, lockedPackRefCommit); err != nil {
				t.Fatal(err)
			}
			fp := remoteCacheFingerprint(dir)
			if reload {
				InvalidateRemoteCacheValidations()
			} else {
				ResetRemoteCacheValidationCache()
			}
			writeTestFile(t, dir, "nested/file", "edited")
			if remoteCacheFingerprint(dir) != fp {
				t.Fatal("nested edit changed fingerprint")
			}
			*status = " M nested/file"
			err := validateFixture(dir, lockedPackRefCommit)
			if reload {
				if err == nil || !strings.Contains(err.Error(), "local worktree changes") || *calls != 4 {
					t.Fatalf("reload: calls=%d err=%v", *calls, err)
				}
			} else if err != nil || *calls != 2 {
				t.Fatalf("fresh process: calls=%d err=%v", *calls, err)
			}
		})
	}
	for _, reset := range []bool{false, true} {
		name := map[bool]string{false: "memory_checks_HEAD", true: "receipt_checks_HEAD"}[reset]
		t.Run(name, func(t *testing.T) {
			dir, calls, _ := validationFixture(t)
			if err := validateFixture(dir, lockedPackRefCommit); err != nil {
				t.Fatal(err)
			}
			fp := remoteCacheFingerprint(dir)
			path := filepath.Join(dir, ".git", "HEAD")
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			wrong := strings.Repeat("f", 40)
			writeTestFile(t, dir, ".git/HEAD", wrong)
			if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
				t.Fatal(err)
			}
			if remoteCacheFingerprint(dir) != fp {
				t.Fatal("HEAD edit changed fingerprint")
			}
			runRepoCacheGit = func(_ string, _ ...string) (string, error) { *calls++; return wrong, nil }
			if reset {
				ResetRemoteCacheValidationCache()
			}
			if err := validateFixture(dir, lockedPackRefCommit); err == nil || !strings.Contains(err.Error(), "is checked out at") || *calls != 3 {
				t.Fatalf("changed HEAD: calls=%d err=%v", *calls, err)
			}
		})
	}
	t.Run("execution_reuses_full_receipt_until_reload", func(t *testing.T) {
		dir, calls, status := validationFixture(t)
		writeTestFile(t, dir, "nested/run.sh", "original")
		if err := validateFixture(dir, lockedPackRefCommit); err != nil {
			t.Fatal(err)
		}
		old := runRepoCacheGitContext
		runRepoCacheGitContext = func(_ context.Context, dir string, args ...string) (string, error) {
			return runRepoCacheGit(dir, args...)
		}
		t.Cleanup(func() { runRepoCacheGitContext = old })
		ResetRemoteCacheValidationCache()
		// A fresh process resolves ownership even when it reuses a receipt.
		rememberRemoteCache(validationTestSource, filepath.Dir(dir), dir, lockedPackRefCommit)
		executable := filepath.Join(dir, "nested/run.sh")
		if err := cacheexec.ValidateCachedExecutable(context.Background(), executable); err != nil || *calls != 2 {
			t.Fatalf("fresh execution: calls=%d err=%v", *calls, err)
		}
		fp := remoteCacheFingerprint(dir)
		writeTestFile(t, dir, "nested/run.sh", "edited")
		if remoteCacheFingerprint(dir) != fp {
			t.Fatal("nested edit changed fingerprint")
		}
		*status = " M nested/run.sh"
		InvalidateRemoteCacheValidations()
		if err := cacheexec.ValidateCachedExecutable(context.Background(), executable); err == nil || !strings.Contains(err.Error(), "local worktree changes") || *calls != 4 {
			t.Fatalf("execution after reload: calls=%d err=%v", *calls, err)
		}
		*status = ""
		if err := cacheexec.ValidateCachedExecutable(context.Background(), executable); err != nil || *calls != 6 {
			t.Fatalf("repaired execution: calls=%d err=%v", *calls, err)
		}
		ResetRemoteCacheValidationCache()
		if err := validateFixture(dir, lockedPackRefCommit); err != nil || *calls != 6 {
			t.Fatalf("execution did not publish FULL receipt: calls=%d err=%v", *calls, err)
		}
	})
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
