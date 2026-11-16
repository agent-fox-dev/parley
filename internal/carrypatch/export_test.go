package carrypatch

import "context"

// DeleteBackupRef is deleteBackupRef, which an add runs once its
// transaction committed, whatever became of its request.
func DeleteBackupRef(ctx context.Context, c *Component, slug, branch string) {
	c.deleteBackupRef(ctx, slug, branch)
}
