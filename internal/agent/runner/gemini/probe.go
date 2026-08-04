package gemini

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/tphakala/alfred/internal/agent/runner/runnerutil"
)

// ProbeStreamJSON runs `<binaryPath> --help` with a short timeout and checks
// for the `--output-format stream-json` capability in the combined output.
// Used at worker startup to refuse to register the Gemini runner if the
// installed CLI does not support stream-json. The stream-json option landed
// in Gemini CLI via PR #10883; older releases lack it.
func ProbeStreamJSON(binaryPath string) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, binaryPath, "--help")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return false, fmt.Errorf("probe gemini: %w (stderr tail: %s)", err, runnerutil.TruncateForError(stderr.String()))
	}

	combined := stdout.String() + stderr.String()
	return strings.Contains(combined, "stream-json"), nil
}
