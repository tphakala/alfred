package secrets

// SecureDo executes fn in a context intended for sensitive operations.
// Currently a no-op wrapper; reserved for future memory-locking or other
// hardening (e.g. mlock).
func SecureDo(fn func()) {
	if fn == nil {
		return
	}
	fn()
}
