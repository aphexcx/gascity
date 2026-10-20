package events

import (
	"bufio"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// WalkContext visits retained events in sequence order, scanning each archive
// and then the active journal once. It keeps no event history in memory.
// Malformed lines are skipped, matching ReadFiltered. Cancellation is checked
// between records, including within an archive. A visitor error stops the walk.
func WalkContext(ctx context.Context, path string, visit func(Event) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	dir := filepath.Dir(path)
	archives, err := archiveFilesIn(dir)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("listing event archives: %w", err)
	}
	for _, archive := range archives {
		if err := walkEventFile(ctx, filepath.Join(dir, archive.Basename), true, visit); err != nil {
			return fmt.Errorf("reading archive %q: %w", archive.Basename, err)
		}
	}
	return walkEventFile(ctx, path, false, visit)
}

func walkEventFile(ctx context.Context, path string, compressed bool, visit func(Event) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f, err := os.Open(path)
	if err != nil {
		if !compressed && os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("reading events: %w", err)
	}
	defer f.Close() //nolint:errcheck // read-only file
	var r io.Reader = f
	if compressed {
		gr, err := gzip.NewReader(f)
		if err != nil {
			return fmt.Errorf("gunzip: %w", err)
		}
		defer gr.Close() //nolint:errcheck // read-only stream
		r = gr
	}
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !scanner.Scan() {
			break
		}
		var event Event
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			continue
		}
		if err := visit(event); err != nil {
			return err
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("scanning events: %w", err)
	}
	return ctx.Err()
}
