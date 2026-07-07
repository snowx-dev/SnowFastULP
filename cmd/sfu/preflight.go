package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/snowx-dev/SnowFastULP/internal/ulpengine"
	"golang.org/x/term"
)

// best-effort "will this fit?" check + interactive prompt. estimates are
// upper bounds so a "looks tight" warning is usually a false positive,
// but a clean pass is reliable enough to skip without nagging.
//
// returns (ok, err):
//   - ok=true: proceed (clean, user said y, or non-tty stdin auto-continued)
//   - ok=false: user said n
//   - err: stdin I/O failure or ctx canceled

// assumed plaintext -> zstd ratio. real is ~4-8x on ULP text, 5 is a
// safe middle estimate (projects bigger than reality, errs toward warning)
const diskRatioCompressed = 5.0

// multiplicative margin: warn if free < estimate × 1.05. keeps the
// check from firing on near-misses
const diskSafetySlack = 1.05

// cap garbage responses. previous "anything not 'n' is yes" accepted
// tab-completion noise as confirmation
const promptMaxAttempts = 3

// runs the check + optional Y/n prompt. interactive=false auto-continues
// w/ warning still printed (pipe/CI path). ctx aborts the prompt early
// on ctrl-c so users get 130 exit instead of a stuck read
func preflightCheck(ctx context.Context, r *ulpengine.Resolved, interactive bool, stdin io.Reader, out io.Writer) (bool, error) {
	if r == nil || r.TotalInputs <= 0 {
		return true, nil
	}

	outNeed, tempNeed := estimateNeeds(r)
	outDir := filepath.Dir(r.Cfg.Output)
	tempDir := r.TempDir

	warning := buildDiskWarning(outDir, tempDir, outNeed, tempNeed)
	if warning == "" {
		return true, nil
	}

	fmt.Fprintln(out, warning)

	if !interactive {
		fmt.Fprintln(out, "stdin is not a tty: continuing anyway.")
		return true, nil
	}

	return promptUserResponse(ctx, stdin, out)
}

// strict-yes [Y/n] loop, max promptMaxAttempts garbage responses then bail.
// blocking read runs on a goroutine so ctx.Done() can preempt it
func promptUserResponse(ctx context.Context, stdin io.Reader, out io.Writer) (bool, error) {
	type result struct {
		line string
		err  error
	}
	reader := bufio.NewReader(stdin)
	for attempt := 0; attempt < promptMaxAttempts; attempt++ {
		fmt.Fprint(out, "continue anyway? [Y/n]: ")
		// on ctx cancel goroutine leaks until stdin closes, fine b/c main exits
		ch := make(chan result, 1)
		go func() {
			line, err := reader.ReadString('\n')
			ch <- result{line: line, err: err}
		}()
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case res := <-ch:
			if res.err != nil && res.err != io.EOF {
				return false, res.err
			}
			ans := strings.ToLower(strings.TrimSpace(res.line))
			switch ans {
			case "", "y", "yes":
				return true, nil
			case "n", "no":
				return false, nil
			}
			if res.err == io.EOF {
				// EOF before valid answer, dont loop forever
				return false, fmt.Errorf("invalid response after %d attempt(s) (stdin closed)", attempt+1)
			}
			fmt.Fprintf(out, "please answer y or n (got %q)\n", ans)
		}
	}
	return false, fmt.Errorf("invalid response after %d attempts", promptMaxAttempts)
}

// upper bound on output + shard temp bytes.
//
//	-zst:        output ~= totalInputs / diskRatioCompressed
//	plain:       output ~= totalInputs (dedup only shrinks)
//	bucketed:    temp ~= totalInputs (one copy of every parsed line)
//	fast path:   temp = 0 (no shards)
//	-od:         temp += ~8 B/dest key + ~8 B/output line. dest term peeks
//	             sidecar headers, missing/unreadable estimated at size/10
func estimateNeeds(r *ulpengine.Resolved) (outBytes, tempBytes int64) {
	if r.Cfg.Compress {
		outBytes = int64(float64(r.TotalInputs) / diskRatioCompressed)
	} else {
		outBytes = r.TotalInputs
	}
	if !r.UseFastPath {
		tempBytes = r.TotalInputs
	}
	if r.Cfg.DestDedup {
		tempBytes += estimateODOverhead(r)
	}
	return
}

// -od overhead: dest_keys/ bytes + own-output .idx upper bound.
// 8 B per dest key + totalInputs/50 for our own keys (~50 B avg ULP line × 8).
// sidecar errors absorbed silently, preflight is a hint not a gate
func estimateODOverhead(r *ulpengine.Resolved) int64 {
	if r == nil || !r.Cfg.DestDedup {
		return 0
	}
	destKeyBytes := ulpengine.EstimateDestKeyBytes(r.Cfg.DestDedupDir, r.Cfg.RunStamp)
	// totalInputs × 8 / 50 = totalInputs × 0.16
	outputIdxBytes := r.TotalInputs * ulpengine.SidecarKeyBytes / 50
	return destKeyBytes + outputIdxBytes
}

// formatted warning, "" when both vols have enough headroom (after slack).
// same-vol output/temp summed before compare, distinct vols checked
// independently and warnings concatenated
func buildDiskWarning(outDir, tempDir string, outNeed, tempNeed int64) string {
	type vol struct {
		path string
		need int64
	}

	var vols []vol
	if tempNeed > 0 && !sameVolume(outDir, tempDir) {
		vols = []vol{{outDir, outNeed}, {tempDir, tempNeed}}
	} else {
		// same vol (or no temp), one combined check. outDir always exists
		// at this point and df reports the same either way
		vols = []vol{{outDir, outNeed + tempNeed}}
	}

	var b strings.Builder
	for _, v := range vols {
		freeRaw, err := diskFree(v.path)
		if err != nil {
			// cant measure -> dont warn. silent skip beats alarming
			// the user about a good run on an unstatfsable fs
			continue
		}
		need := int64(float64(v.need) * diskSafetySlack)
		if int64(freeRaw) >= need {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b,
			"warning: estimated need %s on %s but only %s free (after %d%% slack)",
			humanBytes(need), v.path, humanBytes(int64(freeRaw)),
			int((diskSafetySlack-1)*100),
		)
	}
	return b.String()
}

// true if r is os.Stdin AND that fd is a terminal. pipes/null/test
// readers return false so caller auto-continues without prompting
func isStdinTTY(r io.Reader) bool {
	f, ok := r.(*os.File)
	if !ok {
		return false
	}
	return term.IsTerminal(int(f.Fd()))
}
