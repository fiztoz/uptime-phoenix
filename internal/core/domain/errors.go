package domain

import "errors"

// Common domain errors used across the application.
var (
	ErrNotFound     = errors.New("resource not found")
	ErrUnauthorized = errors.New("unauthorized")
	ErrValidation   = errors.New("validation error")
	ErrConflict     = errors.New("resource conflict")
	ErrInternal     = errors.New("internal error")
)
