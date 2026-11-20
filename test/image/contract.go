package image

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// contractScript is what an agent does first in a sandbox: it makes a
// repository in a writable directory, commits one file and reads the commit
// back. It runs as the image's default user, since that is who the sandbox
// runs as.
const contractScript = `cd "$(mktemp -d)" && git init -q . && echo x > f && git add f && git -c user.name=t -c user.email=t@x commit -q -m m && git rev-parse HEAD`

// contractLimit bounds the check, which may pull the image first.
const contractLimit = 10 * time.Minute

// objectName is the output of git rev-parse HEAD: a SHA-1 or a SHA-256
// object name.
var objectName = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`)

// CheckContract reports whether the image ref satisfies the image contract
// of a sandbox: in a container of that image, run by engine as the image's
// default user, git init, a commit of one file and git rev-parse HEAD all
// succeed. The error names the image and git, and holds what the container
// printed; the script runs git quietly, so that is short.
func CheckContract(engine, ref string) error {
	ctx, cancel := context.WithTimeout(context.Background(), contractLimit)
	defer cancel()
	cmd := exec.CommandContext(ctx, engine, "run", "--rm", ref, "sh", "-c", contractScript)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("image %s does not satisfy the image contract: git init, a commit and git rev-parse HEAD failed as its default user: %w\n%s",
			ref, err, strings.TrimSpace(stdout.String()+stderr.String()))
	}
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if head := lines[len(lines)-1]; !objectName.MatchString(head) {
		return fmt.Errorf("image %s does not satisfy the image contract: git rev-parse HEAD printed %q, not an object name", ref, head)
	}
	return nil
}
