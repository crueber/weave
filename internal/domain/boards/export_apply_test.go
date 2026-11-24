package boards

// SetApplyHook lets the external tests fail board.template.apply at a named stage (atomicity).
func SetApplyHook(f func(stage string) error) { applyHook = f }
