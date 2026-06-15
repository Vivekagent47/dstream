package ingest

import "errors"

var (
	ErrUnknownBodyRef = errors.New("ingest: unknown body ref scheme")
	ErrSourceNotFound = errors.New("ingest: source not found")
	ErrBodyTooLarge   = errors.New("ingest: body too large")
)
