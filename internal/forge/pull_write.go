package forge

import "context"

// commentOnPull posts a comment in the conversation of a pull request.
func commentOnPull(ctx context.Context, call *Call) (written, error) {
	return postComment(ctx, call, true)
}
