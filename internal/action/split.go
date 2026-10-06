package action

import "context"

// RunCmdSplit is RunCmd that also returns stdout alone (stderr excluded), for
// callers that parse the command's JSON output. Both are masked and capped.
func RunCmdSplit(ctx context.Context, argv []string, opts SandboxOptions, secrets []string) (exit int, output, stdout []byte, err error) {
	return runCommand(ctx, argv, opts, secrets, nil, true)
}
