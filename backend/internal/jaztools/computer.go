package jaztools

import (
	"github.com/wins/jaz/backend/internal/computercontrol"
	"github.com/wins/jaz/backend/internal/settings"
	"github.com/wins/jaz/backend/internal/storage"
)

func (s *Service) SetComputer(store storage.SettingsStorage, backend computercontrol.Backend) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.computerSettings = store
	s.computerBackend = backend
	for _, current := range s.slots() {
		if current.slot.computerTools {
			computercontrol.RemoveMCPTools(current.slot.server)
			current.slot.computerTools = false
		}
		s.syncComputerToolsFor(current.slot, current.surface)
	}
}

func (s *Service) syncComputerToolsFor(slot *serverSlot, surface toolSurface) {
	if surface.workerOnly() || slot.server == nil || s.computerSettings == nil {
		return
	}
	enabled := settings.ComputerEnabled(s.computerSettings)
	if enabled == slot.computerTools {
		return
	}
	if enabled {
		computercontrol.AddMCPTools(slot.server, s.computerBackend)
	} else {
		computercontrol.RemoveMCPTools(slot.server)
	}
	slot.computerTools = enabled
}
