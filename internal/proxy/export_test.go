package proxy

import "sync"

// Transports returns the handler's internal transport pool for testing purposes.
func (h *Handler) Transports() *sync.Map {
	return &h.transports
}
