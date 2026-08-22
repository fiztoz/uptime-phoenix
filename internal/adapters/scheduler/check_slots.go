package scheduler

import "context"

// defaultCheckConcurrency bounds how many monitor checks run at once.
//
// A restart (or every monitor sharing the same interval) used to `go runCheck`
// for every due monitor in the same tick. At 60 monitors that is 60 concurrent
// heartbeat writes plus, for mysql/mariadb checkers, 60 extra sessions on the
// target — enough to exhaust MariaDB (Error 1040) while the dashboard is also
// loading. Sized under the default MariaDB pool (10) so Record() does not queue
// behind the checkers.
const defaultCheckConcurrency = 8

type checkSlots struct {
	sem chan struct{}
}

func newCheckSlots(n int) *checkSlots {
	if n < 1 {
		n = defaultCheckConcurrency
	}
	return &checkSlots{sem: make(chan struct{}, n)}
}

func (s *checkSlots) acquire(ctx context.Context) bool {
	select {
	case s.sem <- struct{}{}:
		return true
	case <-ctx.Done():
		return false
	}
}

func (s *checkSlots) release() {
	<-s.sem
}
