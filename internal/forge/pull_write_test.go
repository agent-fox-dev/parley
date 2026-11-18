package forge_test

import "testing"

func TestCommentOnPull_PostsInTheConversation(t *testing.T) {
	t.Parallel()
	w := forgesWorld(t)
	for _, f := range []struct{ name, slug, repo, sent string }{
		{"GitHub", "demo", ghRepo, ghRepo + "/issues/12/comments"},
		{"GitLab", "gl", glSubWidgets, glSubWidgets + "/merge_requests/12/notes"},
	} {
		t.Run(f.name, func(t *testing.T) {
			w.wantComment(t, f.slug, "/pulls/12/comments", f.repo, f.sent, 12, "LGTM")
		})
	}
}
