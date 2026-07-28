package ports

import "time"

// Clock abstracts time operations for testability.
type Clock interface {
	Now() time.Time
}
