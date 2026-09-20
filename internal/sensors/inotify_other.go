//go:build !linux

package sensors

import (
	"context"

	"github.com/trouble-agent/trouble/internal/types"
)

// There is no non-Linux file-watch implementation: inotify is a Linux
// interface and darwin's kqueue/FSEvents and Windows' ReadDirectoryChangesW are
// NOT implemented here. The sensor is reported UNavailable with the gap named,
// because a silently absent watch set looks exactly like "nothing changed on
// disk" (SPEC-03 §3.3, SPEC-12 §3.5a).
const inotifyPlatformReason = "platform gap: inotify(7) is Linux-only and no non-Linux file-watch implementation exists (SPEC-12 §3.5a)"

func (s *Sensors) startInotify(ctx context.Context) error {
	if !s.cfg.inotify.enabled {
		s.setSensor(types.SenInotify, false, false, "disabled by configuration: sensors.inotify.enabled=false")
		return nil
	}
	s.setSensor(types.SenInotify, false, true, inotifyPlatformReason)
	return nil
}

// stopInotify has nothing to stop: no watch set was ever established.
func (s *Sensors) stopInotify() {}
