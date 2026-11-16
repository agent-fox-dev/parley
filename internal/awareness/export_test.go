package awareness

// FailNextCommit makes the transaction of the next event the worker
// processes fail with err, as a store that fails at commit would.
func FailNextCommit(c *Component, err error) {
	c.beforeCommit = func() error {
		c.beforeCommit = nil
		return err
	}
}
