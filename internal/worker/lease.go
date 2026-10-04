package worker

import (
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/google/uuid"
)

// defaultLeaseTTL is how long an attempt's lease survives without a renewal
// (research C1). The cancel poll renews it every couple of seconds, so a live
// run holds its lease indefinitely and a dead one is findable within this
// window.
const defaultLeaseTTL = 30 * time.Second

// leaseTTL returns the lease duration for a run that polls every poll: long
// enough to ride out a few failed renewals, and never shorter than the
// default, so a caller who lengthens CancelPoll cannot make its own live run
// look dead to recovery.
func leaseTTL(poll time.Duration) time.Duration {
	if ttl := 3 * poll; ttl > defaultLeaseTTL {
		return ttl
	}
	return defaultLeaseTTL
}

// processLeaseOwner identifies this process in every lease it takes
// (hostname:pid:uuid), so an expired lease names the process to suspect.
// Generated once: a run's owner is its process, not the run — one aidev with
// two concurrent runs claims both attempts under the same identity.
var processLeaseOwner = sync.OnceValue(func() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown-host"
	}
	return fmt.Sprintf("%s:%d:%s", host, os.Getpid(), uuid.Must(uuid.NewV7()))
})
