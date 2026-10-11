package indexer

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
)

// gitBatchContent opens one `git cat-file --batch` process for a commit's
// files: the fingerprint correction read every file of a base with its own
// git process (2,068 processes and 2m10s for one base on the clone).
func gitBatchContent(root, commit string) func(ctx context.Context) (func(ctx context.Context, rel string) ([]byte, bool), func(), error) {
	return func(ctx context.Context) (func(ctx context.Context, rel string) ([]byte, bool), func(), error) {
		cmd := exec.CommandContext(ctx, "git", "-C", root, "cat-file", "--batch")
		stdin, err := cmd.StdinPipe()
		if err != nil {
			return nil, nil, err
		}
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			return nil, nil, err
		}
		if err := cmd.Start(); err != nil {
			return nil, nil, err
		}
		reader := bufio.NewReaderSize(stdout, 1<<16)
		broken := false
		read := func(ctx context.Context, rel string) ([]byte, bool) {
			if broken || ctx.Err() != nil || strings.ContainsAny(rel, "\n") {
				return nil, false
			}
			if _, err := fmt.Fprintf(stdin, "%s:%s\n", commit, rel); err != nil {
				broken = true
				return nil, false
			}
			header, err := reader.ReadString('\n')
			if err != nil {
				broken = true
				return nil, false
			}
			fields := strings.Fields(header)
			// "<oid> missing" or "<name> ambiguous" carry no body.
			if len(fields) != 3 || fields[1] != "blob" {
				return nil, false
			}
			size, err := strconv.Atoi(fields[2])
			if err != nil || size < 0 {
				broken = true
				return nil, false
			}
			body := make([]byte, size+1) // the content and its trailing newline
			if _, err := io.ReadFull(reader, body); err != nil {
				broken = true
				return nil, false
			}
			return body[:size], true
		}
		closeBatch := func() {
			_ = stdin.Close()
			_ = cmd.Wait()
		}
		return read, closeBatch, nil
	}
}
