package web

// Shared error message literals (go:S1192 — duplicated string literals).
const (
	// errMsgInternal is the generic API error message (no internals leak).
	errMsgInternal = "internal error"
	// errMsgNotFound is the generic API 404 message.
	errMsgNotFound = "not found"
	// errMsgBadRequest is the generic 400 body.
	errMsgBadRequest = "bad request"
	// errMsgServerErr is the generic 500 body for page handlers.
	errMsgServerErr = "internal server error"
)
